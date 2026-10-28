package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

// DefaultBackupMaxBytes bounds the logical SQLite snapshot, not main+WAL.
const DefaultBackupMaxBytes int64 = 1 << 30

// HardBackupMaxBytes is the non-disableable upper bound for operator overrides.
const HardBackupMaxBytes int64 = 4 << 30

// BackupMaxBytes validates a per-operation byte limit. Zero selects the default.
func BackupMaxBytes(value int64) (int64, error) {
	if value == 0 {
		return DefaultBackupMaxBytes, nil
	}
	if value < 0 || value > HardBackupMaxBytes {
		return 0, fmt.Errorf("backup limit %d bytes refused: must be positive and at most %d bytes", value, HardBackupMaxBytes)
	}
	return value, nil
}

func backupSizeError(size, limit int64) error {
	return fmt.Errorf("backup size %d bytes exceeds limit %d bytes (raise --max-database-bytes within the 4 GiB maximum)", size, limit)
}

// A read transaction pins one committed SQLite view across bounded backup_step
// calls. Never use immutable=1 on a live source, even if its WAL is absent now.
// The driver opens the source read-only; only the new private destination is
// writable. SHM reader locks may change, but main/WAL data is never written.
func snapshotOnlineDatabase(ctx context.Context, path string, maxBytes int64) (snapshot string, cleanup func() error, resultErr error) {
	return snapshotOnlineDatabaseSteps(ctx, path, maxBytes, func(backup *sqlite.Backup) (bool, error) {
		return backup.Step(128)
	})
}

// Separate the step operation so consistency tests can coordinate real writer
// commits between real backup steps, without sleeps or scheduler assumptions.
func snapshotOnlineDatabaseSteps(ctx context.Context, path string, maxBytes int64, step func(*sqlite.Backup) (bool, error)) (snapshot string, cleanup func() error, resultErr error) {
	return snapshotOnlineDatabaseWithSpace(ctx, path, maxBytes, step, backupVolumeFreeBytes)
}

func snapshotOnlineDatabaseWithSpace(ctx context.Context, path string, maxBytes int64, step func(*sqlite.Backup) (bool, error), free func(string) (int64, error)) (snapshot string, cleanup func() error, resultErr error) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		f, _, err := openSQLiteSourceFile(path+suffix, suffix == "")
		if err != nil {
			return "", nil, err
		}
		if f != nil {
			if err := f.Close(); err != nil {
				return "", nil, err
			}
		}
	}
	if err := verifySQLiteWALHeader(path); err != nil {
		return "", nil, err
	}
	parent := filepath.Dir(path)
	volume, err := lockOnlineBackupVolume(parent)
	if err != nil {
		return "", nil, err
	}
	cleanup = volume.Close
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, cleanup())
		}
	}()
	if err := sweepOnlineBackupStaging(parent, time.Now()); err != nil {
		log.Printf("WARNING: interrupted database snapshot cleanup incomplete; inspect/remove the named stale entry: %v", err)
	}

	db, err := sql.Open("sqlite", currentDatabaseDSN(path, "ro", false))
	if err != nil {
		return "", cleanup, err
	}
	defer func() { resultErr = errors.Join(resultErr, db.Close()) }()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", cleanup, err
	}
	defer func() { resultErr = errors.Join(resultErr, conn.Close()) }()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", cleanup, err
	}
	defer func() { _ = tx.Rollback() }()
	var pages, pageSize int64
	if err := tx.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return "", cleanup, err
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return "", cleanup, err
	}
	if pages <= 0 || pageSize < 512 || pageSize > 65536 || pages > (1<<32)-1 {
		return "", cleanup, errors.New("backup page inventory refused")
	}
	if size := pages * pageSize; size > maxBytes {
		return "", cleanup, backupSizeError(size, maxBytes)
	}
	// Inventory the pinned view before creating any staging file. Keep space for
	// live WAL/activity even when the operator raises the logical database limit.
	size := pages * pageSize
	guard := backupSpaceGuard{volume: parent, size: size, reserve: max(backupSpaceMinimumReserve, size/4), free: free}
	if err := guard.check(); err != nil {
		return "", cleanup, err
	}
	dir, stageCleanup, err := newOnlineBackupStaging(parent)
	if err != nil {
		return "", cleanup, err
	}
	volumeCleanup := cleanup
	cleanup = func() error { return errors.Join(stageCleanup(), volumeCleanup()) }
	snapshot = filepath.Join(dir, "oberth.sqlite")
	f, err := os.OpenFile(snapshot, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) // #nosec G304 -- fixed filename in fresh private directory.
	if err != nil {
		return "", cleanup, err
	}
	if err := f.Close(); err != nil {
		return "", cleanup, err
	}
	guard.snapshot = snapshot

	err = conn.Raw(func(raw any) (resultErr error) {
		driver, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite online backup unavailable")
		}
		backup, err := driver.NewBackup(snapshot)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, backup.Finish()) }()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := guard.check(); err != nil {
				return err
			}
			more, err := step(backup)
			if err != nil {
				return err
			}
			if err := guard.check(); err != nil {
				return err
			}
			if !more {
				return ctx.Err()
			}
		}
	})
	if err != nil {
		return "", cleanup, err
	}
	return snapshot, cleanup, nil
}
