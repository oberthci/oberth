package installer

// Database half of `oberth backup` / `oberth restore`.
//
// Backup streams the server's verified archive through the authenticated admin API.
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

	snapshotAPI = "admin-api"
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
func podSchemaVersion(ctx context.Context, deps Deps, namespace string) (int, error) {
	_, schema, err := adminVersion(ctx, Config{Namespace: namespace}, deps)
	return schema, err
}

// snapshotServerDatabase writes the store's verified database archive
// (oberth.database-backup/v1) to destination and returns how it was taken.
func snapshotServerDatabase(ctx context.Context, deps backupDeps, namespace, destination string) (string, error) {
	limit, err := store.BackupMaxBytes(deps.maxDatabaseBytes)
	if err != nil {
		return "", err
	}
	remote, closeConnection, err := adminAPI(ctx, Config{Namespace: namespace}, deps.Deps)
	if err != nil {
		return "", err
	}
	defer closeConnection()
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) // #nosec G304 -- exclusive private backup staging destination selected by the operator.
	if err != nil {
		return "", err
	}
	transferErr := remote.DownloadAdminBackup(ctx, limit, file)
	if err := errors.Join(transferErr, file.Close()); err != nil {
		_ = os.Remove(destination)
		return "", err
	}
	return snapshotAPI, nil
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
	report, err := store.VerifyBackupArchive(ctx, f, schema, store.Options{Protocol: protocol, BackupMaxBytes: maxBytes, SnapshotTempDir: filepath.Dir(path)})
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
		file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) // #nosec G304 -- exclusive private backup staging destination selected by the operator. // #nosec G304 -- one of two exact names in fresh private directory.
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

// Utility schema output is read from an offline restore init container, not
// by executing commands inside the running server.
func parseUtilitySchema(out []byte) (int, error) {
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, errors.New("empty utility schema output")
	}
	version, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil || version < 1 {
		return 0, errors.New("invalid utility schema output")
	}
	return version, nil
}
