package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
	return fmt.Errorf("backup size %d bytes exceeds limit %d bytes", size, limit)
}

// A read transaction pins one committed SQLite view across bounded backup_step
// calls. Never use immutable=1 on a live source, even if its WAL is absent now.
// The driver opens the source read-only; only the new private destination is
// writable. SHM reader locks may change, but main/WAL data is never written.
func snapshotOnlineDatabase(ctx context.Context, path string, maxBytes int64) (snapshot string, cleanup func() error, resultErr error) {
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
	dir, err := os.MkdirTemp("/tmp", "oberth-online-backup-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() error { return os.RemoveAll(dir) }
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, cleanup())
		}
	}()
	snapshot = filepath.Join(dir, "oberth.sqlite")
	f, err := os.OpenFile(snapshot, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) // #nosec G304 -- fixed filename in fresh private directory.
	if err != nil {
		return "", cleanup, err
	}
	if err := f.Close(); err != nil {
		return "", cleanup, err
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
			more, err := backup.Step(128)
			if err != nil {
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
