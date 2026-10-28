package installer

// Database half of `oberth backup` / `oberth restore`.
//
// Backup: the running server binary takes its own verified snapshot in-pod
// (`oberth backup database`, the store's WriteVerifiedBackup — the semantics
// documented in docs/database-backup.md) and streams the closed archive out
// over kubectl exec stdout. A server released before that subcommand existed
// falls back to a stable copy: both SQLite files are streamed out and must
// hash identically before and after the copy, and the same verified snapshot
// is then taken locally with the online API. Only the compatibility copy
// requires stable before/after hashes (with bounded retries).
//
// Restore: the file is swapped on the server's PersistentVolumeClaim through a
// short-lived pod that mounts the claim — never a host path — so it works on
// any storage class.

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/containerimage"
	"github.com/oberthci/oberth/internal/store"
)

const (
	// serverDatabasePath is the chart's fixed --database flag.
	serverDatabasePath = "/data/oberth.sqlite"
	// serverContainerName is the chart's server container.
	serverContainerName = "oberth"
	// databaseSnapshotAttempts bounds retries when the live database changes
	// while it is being snapshotted.
	databaseSnapshotAttempts = 3

	snapshotInPod      = "in-pod"
	snapshotStableCopy = "stable-copy"
)

// kubectlExecArgs builds a `kubectl exec` argv for one container.
func kubectlExecArgs(contextName, namespace, pod, container string, stdin bool, command ...string) []string {
	if container == serverContainerName {
		command = containerimage.ServerCommand(command)
	}
	args := []string{"exec"}
	if stdin {
		args = append(args, "-i")
	}
	args = append(args, "-c", container)
	if contextName != "" {
		args = append(args, "--context", contextName)
	}
	args = append(args, "-n", namespace, pod, "--")
	return append(args, command...)
}

// podSchemaVersion asks a pod's own oberth binary which schema it writes.
func podSchemaVersion(ctx context.Context, run CommandRunner, contextName, namespace, pod, container string) (int, error) {
	out, err := run(ctx, nil, "kubectl", kubectlExecArgs(contextName, namespace, pod, container, false, "oberth", "version", "--schema")...)
	if err != nil {
		return 0, fmt.Errorf("read server schema version: %w%s", err, commandOutputSuffix(out))
	}
	return parsePodSchema(out)
}
func parsePodSchema(out []byte) (int, error) {
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, errors.New("read server schema version: empty output")
	}
	version, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil || version < 1 {
		return 0, fmt.Errorf("read server schema version: unexpected output %q", strings.TrimSpace(string(out)))
	}
	return version, nil
}

// snapshotServerDatabase writes the store's verified database archive
// (oberth.database-backup/v1) to destination and returns how it was taken.
func snapshotServerDatabase(ctx context.Context, deps backupDeps, namespace, pod string, schema int, protocol store.ProtocolConfig, workDir, destination string) (string, error) {
	limit, err := store.BackupMaxBytes(deps.maxDatabaseBytes)
	if err != nil {
		return "", err
	}
	var lastErr error
	// Preserve the working in-pod path on older servers for ordinary requests.
	// The workstation still verifies the received archive against this limit.
	useLimitFlag := limit != store.DefaultBackupMaxBytes
	for attempt := 1; attempt <= databaseSnapshotAttempts; attempt++ {
		args := kubectlExecArgs(deps.ContextName, namespace, pod, serverContainerName, false,
			"oberth", "backup", "database", "--database", serverDatabasePath)
		if useLimitFlag {
			args = append(args, "--max-database-bytes", strconv.FormatInt(limit, 10))
		}
		_, err := streamBackupFile(ctx, deps, destination, limit+(64<<10), args)
		if err != nil && ctx.Err() == nil && useLimitFlag &&
			strings.Contains(err.Error(), "flag provided but not defined: -max-database-bytes") && limit >= 128<<20 {
			// The old server's fixed 128 MiB cap is stricter. Retry its original
			// invocation before paying for a utility Pod or requiring a stable
			// transfer of the live main/WAL. Keep the local stream/verify bound.
			useLimitFlag = false
			args = args[:len(args)-2]
			_, err = streamBackupFile(ctx, deps, destination, limit+(64<<10), args)
		}
		if err == nil {
			return snapshotInPod, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// An older server with no backup command, an old cap refusal, or a
		// requested bound smaller than its cap needs the stable-copy fallback.
		if strings.Contains(err.Error(), `unknown command "backup"`) ||
			strings.Contains(err.Error(), "flag provided but not defined: -max-database-bytes") ||
			strings.Contains(err.Error(), "backup size refused") {
			_, _ = fmt.Fprintln(deps.Output, "  The deployed server predates bounded online backup; taking a hash-checked stable copy and snapshotting it locally.")
			return snapshotStableCopy, legacyStableCopyDatabase(ctx, deps, namespace, pod, schema, protocol, workDir, destination)
		}
		lastErr = err
		if attempt < databaseSnapshotAttempts {
			_, _ = fmt.Fprintf(deps.Output, "  Database snapshot attempt %d failed (%v); retrying.\n", attempt, err)
			if err := sleepContext(ctx, pollInterval(deps.Deps)); err != nil {
				return "", err
			}
		}
	}
	return "", fmt.Errorf("snapshot server database: %w", lastErr)
}

// stableCopyDatabase streams the main file and WAL out of the pod, accepting
// the copy only when the in-pod hashes before and after it agree with each
// other and with the bytes received, then builds the verified store archive
// locally from that private copy.
func stableCopyDatabase(ctx context.Context, deps backupDeps, namespace, pod, container string, schema int, protocol store.ProtocolConfig, workDir, destination string) error {
	var lastErr error
	for attempt := 1; attempt <= databaseSnapshotAttempts; attempt++ {
		lastErr = stableCopyOnce(ctx, deps, namespace, pod, container, schema, protocol, workDir, destination)
		if lastErr == nil {
			return nil
		}
		if attempt < databaseSnapshotAttempts {
			_, _ = fmt.Fprintf(deps.Output, "  Stable copy attempt %d failed (%v); retrying.\n", attempt, lastErr)
			if err := sleepContext(ctx, pollInterval(deps.Deps)); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("copy server database: %w", lastErr)
}

func podSQLiteHashes(ctx context.Context, deps backupDeps, namespace, pod, container string) (map[string]string, error) {
	command := []string{"sh", "-c", utilitySQLiteHashesScript, "sh", serverDatabasePath, strconv.FormatInt(store.HardBackupMaxBytes, 10)}
	out, err := deps.RunCommand(ctx, nil, "kubectl", kubectlExecArgs(deps.ContextName, namespace, pod, container, false, command...)...)
	if err != nil {
		return nil, fmt.Errorf("hash server database: %w%s", err, commandOutputSuffix(out))
	}
	hashes := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && len(fields[0]) == 64 {
			hashes[fields[1]] = fields[0]
		}
	}
	if hashes[serverDatabasePath] == "" {
		return nil, fmt.Errorf("server database %s not found in pod %s", serverDatabasePath, pod)
	}
	return hashes, nil
}

func stableCopyOnce(ctx context.Context, deps backupDeps, namespace, pod, container string, schema int, protocol store.ProtocolConfig, workDir, destination string) (resultErr error) {
	before, err := podSQLiteHashes(ctx, deps, namespace, pod, container)
	if err != nil {
		return err
	}
	copyDir, err := os.MkdirTemp(workDir, "sqlite-copy-")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(copyDir)) }()
	local := filepath.Join(copyDir, "oberth.sqlite")
	for _, suffix := range []string{"", "-wal"} {
		want, present := before[serverDatabasePath+suffix]
		if !present {
			continue
		}
		command := []string{"cat", serverDatabasePath + suffix}
		args := kubectlExecArgs(deps.ContextName, namespace, pod, container, false, command...)
		// Compatibility staging is bounded independently: a WAL may be
		// larger than the logical database, but is never a backup member.
		sum, err := streamBackupFile(ctx, deps, local+suffix, store.HardBackupMaxBytes, args)
		if err != nil {
			return fmt.Errorf("stream database%s: %w", suffix, err)
		}
		if sum != want {
			return errors.New("server database changed while it was copied")
		}
	}
	after, err := podSQLiteHashes(ctx, deps, namespace, pod, container)
	if err != nil {
		return err
	}
	if len(after) != len(before) {
		return errors.New("server database changed while it was copied")
	}
	for path, sum := range before {
		if after[path] != sum {
			return errors.New("server database changed while it was copied")
		}
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- fixed name inside the private working directory.
	if err != nil {
		return err
	}
	_, writeErr := store.WriteVerifiedBackup(ctx, local, schema, store.Options{Protocol: protocol, BackupMaxBytes: deps.maxDatabaseBytes}, out)
	closeErr := out.Close()
	return errors.Join(writeErr, closeErr)
}

// verifyDatabaseArchive validates a store database archive offline: SQLite
// integrity, foreign keys, the exact schema version and identity, and the
// complete audit hash chain under the recorded audit domain.
func verifyDatabaseArchive(ctx context.Context, path string, schema int, protocol store.ProtocolConfig, maxBytes int64) (store.BackupReport, error) {
	if schema < 3 || schema > store.LatestMigrationVersion() {
		return store.BackupReport{}, fmt.Errorf("database schema %d is not readable by this oberth CLI (schema %d); use an oberth release at least as new as the server that wrote the backup",
			schema, store.LatestMigrationVersion())
	}
	f, err := os.Open(path) // #nosec G304 -- fixed member path inside the private working directory.
	if err != nil {
		return store.BackupReport{}, err
	}
	defer func() { _ = f.Close() }()
	report, err := store.VerifyBackupArchive(ctx, f, schema, store.Options{Protocol: protocol, BackupMaxBytes: maxBytes})
	if err != nil {
		return store.BackupReport{}, fmt.Errorf("database validation under schema identity %q and audit domain %q: %w",
			protocol.SchemaIdentity, protocol.AuditDomain, err)
	}
	return report, nil
}

// --- Restore: swap through a pod mounting the claim ---

// restoreStageDir is the staging directory on the volume. It is private to
// the restore and removed by the swap.
const restoreStageDir = "/data/.oberth-restore"

// stageUploadScript receives one file on stdin into the staging directory and
// prints the size it received. The member name ($1) is one of two fixed
// names: oberth.sqlite and oberth.sqlite-wal. cat must not be the script's last command: the shell then
// execs it, and the exec stream can close before stdin is fully delivered,
// truncating the file while kubectl still exits 0 (#067).
const stageUploadScript = `set -eu
umask 077
mkdir -p /data/.oberth-restore
cat > "/data/.oberth-restore/$1"
wc -c < "/data/.oberth-restore/$1"`

// stageResetScript clears a staging directory left by an interrupted restore.
const stageResetScript = `set -eu
rm -rf /data/.oberth-restore`

// swapScript moves the current database files into pre-restore-<ts>/ and the
// staged ones into place after re-checking their hashes on the volume. $1 is
// the timestamp, $2 and $3 the expected SHA-256 of the main file and WAL ("-"
// when the backup carries no WAL). It prints the result's owner and mode.
const swapScript = `set -eu
cd /data
aside="pre-restore-$1"
printf '%s  %s\n' "$2" .oberth-restore/oberth.sqlite | sha256sum -c - >/dev/null
if [ "$3" != "-" ]; then
  printf '%s  %s\n' "$3" .oberth-restore/oberth.sqlite-wal | sha256sum -c - >/dev/null
fi
if [ -e "$aside" ]; then
  echo "$aside already exists" >&2
  exit 1
fi
mkdir "$aside"
# From here on a failure puts the previous database back, so the server is
# never left without one (or started on an empty one) by a half-done swap.
swapping=1
placed=
rollback() {
  rc=$?
  if [ "$rc" -ne 0 ] && [ -n "$swapping" ]; then
    # Until the restored file is placed, every name still in /data is a
    # previous file: only remove what the restore itself put there.
    if [ -n "$placed" ]; then rm -f oberth.sqlite oberth.sqlite-wal oberth.sqlite-shm; fi
    for f in oberth.sqlite oberth.sqlite-wal oberth.sqlite-shm; do
      if [ -e "$aside/$f" ]; then mv -f "$aside/$f" "$f"; fi
    done
    rmdir "$aside" 2>/dev/null || true
    echo "database swap failed; the previous database was put back" >&2
  fi
}
trap rollback EXIT
for f in oberth.sqlite oberth.sqlite-wal oberth.sqlite-shm; do
  if [ -e "$f" ]; then mv "$f" "$aside/"; fi
done
chmod 0600 .oberth-restore/*
placed=1
mv .oberth-restore/oberth.sqlite oberth.sqlite
if [ -e .oberth-restore/oberth.sqlite-wal ]; then mv .oberth-restore/oberth.sqlite-wal oberth.sqlite-wal; fi
rmdir .oberth-restore
swapping=
echo "` + swapCompleteMarker + `" >&2
stat -c '%u:%g %a' oberth.sqlite`

// swapCompleteMarker is printed once the restored database is in place, so a
// later failure (the ownership check) is reported as "restored database in
// place" rather than "previous database in place".
const swapCompleteMarker = "oberth-restore: database swapped"

// restoredDatabase holds private file paths, never database-sized byte slices.
// The owning backup/restore working directory controls their lifetime.
type restoredDatabase struct {
	Main, WAL             string
	MainSHA256, WALSHA256 string
}

// extractDatabaseFiles streams a verified inner archive into adjacent private
// files. Re-check framing/size even though verification has already succeeded.
func extractDatabaseFiles(ctx context.Context, path, work string, maxBytes int64) (db restoredDatabase, resultErr error) {
	limit, err := store.BackupMaxBytes(maxBytes)
	if err != nil {
		return db, err
	}
	f, err := os.Open(path) // #nosec G304 -- verified archive in private working directory.
	if err != nil {
		return db, err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	dir, err := os.MkdirTemp(work, "restored-db-")
	if err != nil {
		return db, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, os.RemoveAll(dir))
		}
	}()
	reader := tar.NewReader(io.LimitReader(&backupContextReader{Reader: f, ctx: ctx}, limit+(64<<10)))
	var total int64
	seen := map[string]bool{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return db, err
		}
		if seen[header.Name] || header.Typeflag != tar.TypeReg || header.Size < 0 {
			return db, errors.New("database archive entry refused")
		}
		seen[header.Name] = true
		if header.Name == "manifest.json" {
			if header.Size > 8192 {
				return db, errors.New("database manifest refused")
			}
			continue
		}
		if header.Name != "oberth.sqlite" && header.Name != "oberth.sqlite-wal" {
			return db, errors.New("database archive inventory refused")
		}
		if header.Size > limit-total {
			return db, backupLimitError(total+header.Size, limit)
		}
		total += header.Size
		destination := filepath.Join(dir, "oberth.sqlite")
		if header.Name == "oberth.sqlite-wal" {
			destination += "-wal"
		}
		file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) // #nosec G304 -- one of two exact names in fresh private directory.
		if err != nil {
			return db, err
		}
		hash := sha256.New()
		_, copyErr := io.CopyN(io.MultiWriter(file, hash), reader, header.Size)
		if err := errors.Join(copyErr, file.Close()); err != nil {
			return db, err
		}
		if header.Name == "oberth.sqlite" {
			db.Main, db.MainSHA256 = destination, hex.EncodeToString(hash.Sum(nil))
		} else {
			db.WAL, db.WALSHA256 = destination, hex.EncodeToString(hash.Sum(nil))
		}
	}
	if db.Main == "" {
		return db, errors.New("database archive has no oberth.sqlite")
	}
	return db, ctx.Err()
}

// sleepContext waits for d or until ctx ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

const databaseCopyContainer = "database-copy"
const utilitySQLiteHashesScript = `set -eu
for f in "$1" "$1-wal"; do
  if [ -f "$f" ]; then
    size=$(stat -c '%s' "$f")
    if [ "$size" -gt "$2" ]; then
      printf 'backup size %s bytes exceeds limit %s bytes\n' "$size" "$2" >&2
      exit 1
    fi
    sha256sum "$f"
  fi
done`

// Pre-backup-subcommand images also predate native admin file commands. Mount
// their PVC read-only in a pinned utility pod on the server's existing node;
// never fall back to executing a shell inside an old/new server image.
func legacyStableCopyDatabase(ctx context.Context, deps backupDeps, namespace, pod string, schema int, protocol store.ProtocolConfig, workDir, destination string) (result error) {
	server, err := inspectServerDeployment(ctx, deps.Deps, namespace)
	if err != nil {
		return err
	}
	original, err := deps.KubeClient.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return err
	}
	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	if deps.utilityImage != "" {
		server.UtilityImage = deps.utilityImage
	}
	helper := restoreHelperPod("oberth-backup-copy-"+stamp, namespace, stamp, server)
	helper.Labels = map[string]string{"oberth.ci/database-copy": stamp}
	helper.Spec.InitContainers = nil
	helper.Spec.NodeName = original.Spec.NodeName
	helper.Spec.Containers[0].Name = databaseCopyContainer
	for index := range helper.Spec.Containers[0].VolumeMounts {
		helper.Spec.Containers[0].VolumeMounts[index].ReadOnly = true
	}
	for index := range helper.Spec.Volumes {
		if claim := helper.Spec.Volumes[index].PersistentVolumeClaim; claim != nil {
			claim.ReadOnly = true
		}
	}
	pods := deps.KubeClient.CoreV1().Pods(namespace)
	if _, err := pods.Create(ctx, helper, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create legacy backup utility pod: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		grace := int64(0)
		err := pods.Delete(cleanup, helper.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
		if err != nil && !apierrors.IsNotFound(err) {
			result = errors.Join(result, err)
		}
	}()
	if err := waitForPodRunning(ctx, deps.Deps, namespace, "oberth.ci/database-copy="+stamp, time.Minute); err != nil {
		return err
	}
	return stableCopyDatabase(ctx, deps, namespace, helper.Name, databaseCopyContainer, schema, protocol, workDir, destination)
}
