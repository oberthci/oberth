package store

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"

	"github.com/oberthci/oberth/internal/stateport"
	"modernc.org/sqlite"
	"modernc.org/sqlite/vfs"
)

type inspectionFS struct{ file *os.File }

func (f inspectionFS) Open(name string) (fs.File, error) {
	if name != "oberth.sqlite" {
		return nil, fs.ErrNotExist
	}
	info, err := f.file.Stat()
	if err != nil {
		return nil, err
	}
	return &inspectionReadFile{SectionReader: io.NewSectionReader(f.file, 0, info.Size()), info: info}, nil
}

type inspectionReadFile struct {
	*io.SectionReader
	info fs.FileInfo
}

func (f *inspectionReadFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (*inspectionReadFile) Close() error                 { return nil }

func verifyInspection(ctx context.Context, file *os.File, options stateport.Options) (report stateport.BackupReport, resultErr error) {
	name, filesystem, err := vfs.New(inspectionFS{file: file})
	if err != nil {
		return report, databaseError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, filesystem.Close()) }()
	query := url.Values{"vfs": {name}, "mode": {"ro"}, "immutable": {"1"}}
	uri := "file:oberth.sqlite?" + query.Encode()
	limit, _ := options.Limit()
	if err = checkInspectionSize(ctx, uri, limit); err != nil {
		return report, err
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return report, databaseError(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() { resultErr = errors.Join(resultErr, db.Close()) }()
	if err = restoreInspection(ctx, db, uri); err != nil {
		return report, err
	}
	return verifyOpenDatabase(ctx, db, options)
}

func checkInspectionSize(ctx context.Context, uri string, limit int64) (resultErr error) {
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return databaseError(err)
	}
	db.SetMaxOpenConns(1)
	defer func() { resultErr = errors.Join(resultErr, db.Close()) }()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = databaseSize(ctx, tx, limit)
	return err
}

// Native backup copies the immutable archive into a read-write memory database;
// the ordinary verifier still checks CHECK constraints, foreign keys and audit links.
func restoreInspection(ctx context.Context, db *sql.DB, source string) (resultErr error) {
	connection, err := db.Conn(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, connection.Close()) }()
	return connection.Raw(func(raw any) (resultErr error) {
		driver, ok := raw.(interface {
			NewRestore(string) (*sqlite.Backup, error)
		})
		if !ok {
			return refuse(unavailable, nil)
		}
		backup, err := driver.NewRestore(source)
		if err != nil {
			return databaseError(err)
		}
		defer func() { resultErr = errors.Join(resultErr, backup.Finish()) }()
		for {
			if err = ctx.Err(); err != nil {
				return err
			}
			more, err := backup.Step(128)
			if err != nil {
				return databaseError(err)
			}
			if !more {
				return nil
			}
		}
	})
}
