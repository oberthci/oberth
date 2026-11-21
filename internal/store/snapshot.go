package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

type snapshotConfig struct {
	limit    int64
	progress func()
	step     func(*sqlite.Backup) (bool, error)
	free     func(string) (int64, error)
}

func defaultSnapshot(limit int64) snapshotConfig {
	return snapshotConfig{
		limit: limit,
		step:  func(b *sqlite.Backup) (bool, error) { return b.Step(128) },
		free:  freeBytes,
	}
}

func snapshotWith(ctx context.Context, source, destination string, config snapshotConfig) (resultErr error) {
	info, err := os.Lstat(source)
	if err != nil {
		return databaseError(err)
	}
	if !info.Mode().IsRegular() {
		return refuse(invalid, nil)
	}
	db := pool(source, "ro", Options{BusyTimeout: 5 * time.Second}, 1)
	defer func() { resultErr = errors.Join(resultErr, db.Close()) }()
	connection, err := db.Conn(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, connection.Close()) }()
	tx, err := connection.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	size, err := databaseSize(ctx, tx, config.limit)
	if err != nil {
		return err
	}
	guard := spaceGuard{parent: filepath.Dir(source), path: destination, size: size, free: config.free}
	if err := guard.check(); err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return databaseError(err)
	}
	if err := file.Close(); err != nil {
		return databaseError(err)
	}
	return connection.Raw(func(raw any) error {
		driver, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return refuse(unavailable, nil)
		}
		backup, err := driver.NewBackup(destination)
		if err != nil {
			return databaseError(err)
		}
		return copyPages(ctx, backup, guard, config)
	})
}

func databaseSize(ctx context.Context, tx *sql.Tx, limit int64) (int64, error) {
	var pages, pageSize int64
	if err := tx.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, databaseError(err)
	}
	if err := tx.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, databaseError(err)
	}
	if pages <= 0 || pages > 1<<32-1 || pageSize < 512 || pageSize > 65536 {
		return 0, refuse(corrupt, nil)
	}
	size := pages * pageSize
	if size > limit {
		return 0, sizeError(size, limit)
	}
	return size, nil
}

func copyPages(ctx context.Context, backup *sqlite.Backup, guard spaceGuard, config snapshotConfig) (resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, backup.Finish()) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := guard.check(); err != nil {
			return err
		}
		more, err := config.step(backup)
		if err != nil {
			return databaseError(err)
		}
		if err := guard.check(); err != nil {
			return err
		}
		if config.progress != nil {
			config.progress()
		}
		if !more {
			return ctx.Err()
		}
	}
}
