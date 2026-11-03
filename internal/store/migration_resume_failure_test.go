package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

type resumeFailDriver struct{ next atomic.Int32 }

func (d *resumeFailDriver) Open(name string) (driver.Conn, error) {
	c, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return &resumeFailConn{Conn: c, owner: d}, nil
}

type resumeFailConn struct {
	driver.Conn
	owner *resumeFailDriver
}

func (c *resumeFailConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.owner.next.Add(1) == 2 {
		return nil, errors.New("resume begin failed")
	}
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *resumeFailConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
}
func (c *resumeFailConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}
func TestMigrationResumeBeginFailureDoesNotPanic(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	if _, err := s.db.ExecContext(t.Context(), `DELETE FROM schema_migrations WHERE version>=11`); err != nil {
		t.Fatal(err)
	}
	d := &resumeFailDriver{}
	name := fmt.Sprintf("oberth-resume-%d", time.Now().UnixNano())
	sql.Register(name, d)
	db, err := sql.Open(name, s.livePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("resume failure panicked: %v", r)
		}
	}()
	testStore := &Store{db: db, now: time.Now, protocol: s.protocol}
	if err = testStore.migrate(t.Context(), nil); err == nil {
		t.Fatal("missing resume failure")
	}
	if d.next.Load() != 2 {
		t.Fatalf("did not exercise resumed BeginTx: %d, %v", d.next.Load(), err)
	}
}
