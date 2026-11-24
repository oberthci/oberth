package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
)

func verifyDatabase(ctx context.Context, path string, options stateport.Options) (report stateport.BackupReport, resultErr error) {
	db := pool(path, "rw", Options{BusyTimeout: 5 * time.Second}, 1)
	defer func() { resultErr = errors.Join(resultErr, db.Close()) }()
	// Verification only receives private staged copies. The embedded engine
	// skips CHECK-constraint integrity verification for read-only databases.
	if err := checkProtocol(ctx, db, options); err != nil {
		return report, err
	}
	if err := checkSchema(ctx, db); err != nil {
		return report, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return report, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	limit, _ := options.Limit()
	if _, err := databaseSize(ctx, tx, limit); err != nil {
		return report, err
	}
	var integrity string
	if err := tx.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return report, refuse(corrupt, err)
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return report, refuse(corrupt, err)
	}
	found := rows.Next()
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil || found {
		return report, refuse(corrupt, err)
	}
	head, err := verifyAudit(ctx, tx, options.Protocol.AuditDomain)
	if err != nil {
		return report, err
	}
	return stateport.BackupReport{Schema: options.Schema, AuditHead: head, Integrity: true, ForeignKeys: true}, ctx.Err()
}

func checkSchema(ctx context.Context, db *sql.DB) (resultErr error) {
	return checkLayout(ctx, db, baseline)
}

type schemaReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func checkLayout(ctx context.Context, db schemaReader, layout string) (resultErr error) {
	expected, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return databaseError(err)
	}
	expected.SetMaxOpenConns(1)
	defer func() { resultErr = errors.Join(resultErr, expected.Close()) }()
	if _, err := expected.ExecContext(ctx, layout); err != nil {
		return databaseError(err)
	}
	want, err := schemaObjects(ctx, expected)
	if err != nil {
		return err
	}
	got, err := schemaObjects(ctx, db)
	if err != nil {
		return err
	}
	if got != want {
		return refuse(schema, nil)
	}
	return nil
}

func schemaObjects(ctx context.Context, db schemaReader) (string, error) {
	rows, err := db.QueryContext(ctx, `SELECT type,name,tbl_name,coalesce(sql,'') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type,
name`)
	if err != nil {
		return "", databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	result := ""
	for rows.Next() {
		var kind, name, table, statement string
		if err := rows.Scan(&kind, &name, &table, &statement); err != nil {
			return "", databaseError(err)
		}
		result += encode([]string{kind, name, table, statement}) + "\n"
	}
	return result, databaseError(rows.Err())
}
