package installer

// Database half of `oberth backup` / `oberth restore`.
//
// Backup: the running server binary takes its own verified snapshot in-pod
// (`oberth backup database`, the store's WriteVerifiedBackup — the semantics
// documented in docs/database-backup.md) and streams the closed archive out
// over kubectl exec stdout. A server released before that subcommand existed
// falls back to a stable copy: both SQLite files are streamed out and must
// hash identically before and after the copy, and the same verified snapshot
// is then taken locally. Neither path is a racing file copy: a source that
// changes underneath is refused (and retried a bounded number of times).
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
	var lastErr error
	for attempt := 1; attempt <= databaseSnapshotAttempts; attempt++ {
		archive, err := deps.runStdout(ctx, nil, "kubectl", kubectlExecArgs(deps.ContextName, namespace, pod, serverContainerName, false,
			"oberth", "backup", "database", "--database", serverDatabasePath)...)
		if err == nil {
			if writeErr := os.WriteFile(destination, archive, 0o600); writeErr != nil { // #nosec G306 G703 -- fixed name inside the private working directory.
				return "", writeErr
			}
			return snapshotInPod, nil
		}
		if strings.Contains(err.Error(), `unknown command "backup"`) {
			_, _ = fmt.Fprintln(deps.Output, "  The deployed server predates `oberth backup database`; taking a hash-checked stable copy instead.")
			return snapshotStableCopy, stableCopyDatabase(ctx, deps, namespace, pod, schema, protocol, workDir, destination)
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
func stableCopyDatabase(ctx context.Context, deps backupDeps, namespace, pod string, schema int, protocol store.ProtocolConfig, workDir, destination string) error {
	var lastErr error
	for attempt := 1; attempt <= databaseSnapshotAttempts; attempt++ {
		lastErr = stableCopyOnce(ctx, deps, namespace, pod, schema, protocol, workDir, destination)
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

// podSQLiteHashesScript prints `sha256sum` lines for the database and its WAL
// (when present). The path arrives as a positional parameter.
const podSQLiteHashesScript = `for f in "$1" "$1-wal"; do if [ -f "$f" ]; then sha256sum "$f"; fi; done`

func podSQLiteHashes(ctx context.Context, deps backupDeps, namespace, pod string) (map[string]string, error) {
	out, err := deps.RunCommand(ctx, nil, "kubectl", kubectlExecArgs(deps.ContextName, namespace, pod, serverContainerName, false,
		"sh", "-c", podSQLiteHashesScript, "sh", serverDatabasePath)...)
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

func stableCopyOnce(ctx context.Context, deps backupDeps, namespace, pod string, schema int, protocol store.ProtocolConfig, workDir, destination string) (resultErr error) {
	before, err := podSQLiteHashes(ctx, deps, namespace, pod)
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
		body, err := deps.runStdout(ctx, nil, "kubectl", kubectlExecArgs(deps.ContextName, namespace, pod, serverContainerName, false,
			"cat", serverDatabasePath+suffix)...)
		if err != nil {
			return fmt.Errorf("stream %s%s: %w", serverDatabasePath, suffix, err)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != want {
			return errors.New("server database changed while it was copied")
		}
		if err := os.WriteFile(local+suffix, body, 0o600); err != nil { // #nosec G306 G703 -- fixed name inside a fresh private directory.
			return err
		}
	}
	after, err := podSQLiteHashes(ctx, deps, namespace, pod)
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
	_, writeErr := store.WriteVerifiedBackup(ctx, local, schema, store.Options{Protocol: protocol}, out)
	closeErr := out.Close()
	return errors.Join(writeErr, closeErr)
}

// verifyDatabaseArchive validates a store database archive offline: SQLite
// integrity, foreign keys, the exact schema version and identity, and the
// complete audit hash chain under the recorded audit domain.
func verifyDatabaseArchive(ctx context.Context, path string, schema int, protocol store.ProtocolConfig) (store.BackupReport, error) {
	if schema < 3 || schema > store.LatestMigrationVersion() {
		return store.BackupReport{}, fmt.Errorf("database schema %d is not readable by this oberth CLI (schema %d); use an oberth release at least as new as the server that wrote the backup",
			schema, store.LatestMigrationVersion())
	}
	f, err := os.Open(path) // #nosec G304 -- fixed member path inside the private working directory.
	if err != nil {
		return store.BackupReport{}, err
	}
	defer func() { _ = f.Close() }()
	report, err := store.VerifyBackupArchive(ctx, f, schema, store.Options{Protocol: protocol})
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

// stageUploadScript receives one database file on stdin into the staging
// directory. The member name ($1) is one of two fixed names.
const stageUploadScript = `set -eu
umask 077
mkdir -p /data/.oberth-restore
cat > "/data/.oberth-restore/$1"`

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

// restoredDatabase is the database pair extracted from a verified archive.
type restoredDatabase struct {
	Main, WAL             []byte
	MainSHA256, WALSHA256 string
}

// extractDatabaseFiles reads oberth.sqlite (and the optional WAL) out of a
// store database archive that verifyDatabaseArchive has already accepted.
func extractDatabaseFiles(path string) (restoredDatabase, error) {
	f, err := os.Open(path) // #nosec G304 -- fixed member path inside the private working directory.
	if err != nil {
		return restoredDatabase{}, err
	}
	defer func() { _ = f.Close() }()
	var db restoredDatabase
	if err := readStoreArchiveFiles(f, func(name string, body []byte) {
		sum := sha256.Sum256(body)
		switch name {
		case "oberth.sqlite":
			db.Main, db.MainSHA256 = body, hex.EncodeToString(sum[:])
		case "oberth.sqlite-wal":
			db.WAL, db.WALSHA256 = body, hex.EncodeToString(sum[:])
		}
	}); err != nil {
		return restoredDatabase{}, err
	}
	if db.Main == nil {
		return restoredDatabase{}, errors.New("database archive has no oberth.sqlite")
	}
	return db, nil
}

// writeRestoredDatabase writes the pair next to each other for local
// inspection (orphaned-workflow detection).
func writeRestoredDatabase(dir string, db restoredDatabase) (string, error) {
	inspectDir, err := os.MkdirTemp(dir, "restored-db-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(inspectDir, "oberth.sqlite")
	if err := os.WriteFile(path, db.Main, 0o600); err != nil { // #nosec G306 G703 -- fixed name inside a fresh private directory.
		return "", err
	}
	if db.WAL != nil {
		if err := os.WriteFile(path+"-wal", db.WAL, 0o600); err != nil { // #nosec G306 G703 -- fixed name inside a fresh private directory.
			return "", err
		}
	}
	return path, nil
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

// readStoreArchiveFiles hands each SQLite member of a store database archive
// to visit. The archive was verified by store.VerifyBackupArchive first; the
// bounds here only keep a damaged file from exhausting memory.
func readStoreArchiveFiles(r io.Reader, visit func(name string, body []byte)) error {
	reader := tar.NewReader(io.LimitReader(r, maxBackupMemberBytes))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read database archive: %w", err)
		}
		if header.Name != "oberth.sqlite" && header.Name != "oberth.sqlite-wal" {
			continue
		}
		if header.Size < 0 || header.Size > maxBackupMemberBytes {
			return errors.New("database archive member is oversized")
		}
		body := make([]byte, header.Size)
		if _, err := io.ReadFull(reader, body); err != nil {
			return fmt.Errorf("read database archive: %w", err)
		}
		visit(header.Name, body)
	}
}
