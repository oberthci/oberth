package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/oberthci/oberth/internal/model"
)

// Interleave a real WAL writer exactly after the queue query finishes, without
// sleeps or a production hook. The dated query must retain the first snapshot.
type flatRunConnector struct {
	driver     driver.Driver
	dsn        string
	afterQueue func() error
}

func (c flatRunConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &flatRunConn{Conn: conn, afterQueue: c.afterQueue}, nil
}
func (c flatRunConnector) Driver() driver.Driver { return c.driver }

type flatRunConn struct {
	driver.Conn
	afterQueue func() error
}

func (c *flatRunConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *flatRunConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	if strings.Contains(query, "WITH queue AS") && c.afterQueue != nil {
		after := c.afterQueue
		c.afterQueue = nil
		return &flatRunRows{Rows: rows, afterClose: after}, nil
	}
	return rows, nil
}

type flatRunRows struct {
	driver.Rows
	afterClose func() error
}

func (r *flatRunRows) Close() error {
	if err := r.Rows.Close(); err != nil {
		return err
	}
	return r.afterClose()
}

func TestFlatRunsUseOneSnapshotAcrossAdmission(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "snapshot.sqlite")
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	writer, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	repo := createRepo(t, writer)
	run := projectionRun(t, writer, repo.ID, "main", projectionSourceSHA)
	dsn, err := immediateTransactionDSN(path, defaultBusyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	admitted := false
	db := sql.OpenDB(flatRunConnector{driver: &sqlite.Driver{}, dsn: dsn, afterQueue: func() error {
		now = now.Add(time.Minute)
		_, err := writer.MarkRunAdmitted(ctx, run.ID)
		admitted = err == nil
		return err
	}})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	reader := &Store{db: db}
	rows, err := reader.ListRecentRuns(ctx, model.RunListFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !admitted || len(rows) != 1 || rows[0].ID != run.ID || rows[0].StartedAt != nil || rows[0].QueuePosition != 1 {
		t.Fatalf("admitted=%t rows=%+v, want one queued row from the original snapshot", admitted, rows)
	}
	current, err := writer.Run(ctx, run.ID)
	if err != nil || current.StartedAt == nil {
		t.Fatalf("writer admission=%+v, %v", current, err)
	}
	rows, err = reader.ListRecentRuns(ctx, model.RunListFilter{Limit: 1})
	if err != nil || len(rows) != 1 || rows[0].ID != run.ID || rows[0].StartedAt == nil || rows[0].QueuePosition != 0 {
		t.Fatalf("next snapshot=%+v, %v; want the admitted row", rows, err)
	}
}
