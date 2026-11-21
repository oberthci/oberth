package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestIndependentFreshReservedReaderPools(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	for _, pool := range []struct {
		db   *sql.DB
		size int
	}{{s.writer, 1}, {s.readers, 8}, {s.auditReaders, 2}, {s.authReaders, 4}, {s.readiness, 1}, {s.idle, 1}} {
		if pool.db.Stats().MaxOpenConnections != pool.size {
			t.Fatal("pool size", pool.size)
		}
	}
	if cap(s.protocols[0]) != 3 || cap(s.protocols[1]) != 3 {
		t.Fatal("protocol consumed reserve")
	}
	var held []*sql.Conn
	for range 4 {
		connection, err := s.authReaders.Conn(t.Context())
		requireOK(t, err)
		held = append(held, connection)
	}
	requireOK(t, s.Readiness(t.Context()))
	for _, connection := range held {
		requireOK(t, connection.Close())
	}
	for range 3 {
		s.protocols[0] <- struct{}{}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.Authenticate(ctx, 0, hashBytes([]byte("unissued")))
	if !errors.Is(err, context.Canceled) {
		t.Fatal("protocol wait ignored cancellation", err)
	}
	for range 3 {
		<-s.protocols[0]
	}
	assertConnectionSettings(t, s)
}

func assertConnectionSettings(t *testing.T, s *Store) {
	t.Helper()
	for _, tc := range []struct {
		db     *sql.DB
		pragma string
		want   int
	}{{s.writer, "foreign_keys", 1}, {s.writer, "synchronous", 2}, {s.writer, "busy_timeout", 0}, {s.readers, "query_only", 1}, {s.authReaders, "query_only", 1}, {s.auditReaders, "query_only", 1}} {
		var got int
		requireOK(t, tc.db.QueryRowContext(t.Context(), "PRAGMA "+tc.pragma).Scan(&got))
		if got != tc.want {
			t.Fatal(tc.pragma, got)
		}
	}
	var journal string
	requireOK(t, s.writer.QueryRowContext(t.Context(), `PRAGMA journal_mode`).Scan(&journal))
	if journal != "wal" {
		t.Fatal("journal mode", journal)
	}
}

func TestCallerDeadlineBoundsIndependentWriterContention(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	second, err := Open(t.Context(), s.path, s.options)
	requireOK(t, err)
	defer func() { requireOK(t, second.Close()) }()
	held, err := s.writer.BeginTx(t.Context(), nil)
	requireOK(t, err)
	defer func() { _ = held.Rollback() }()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- second.RegisterUpstream(ctx, stateport.Action{Actor: "operator"}, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"})
	}()
	watchdog, stop := context.WithTimeout(t.Context(), 2*time.Second)
	defer stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("deadline cause lost", err)
		}
	case <-watchdog.Done():
		t.Fatal("busy timeout exceeded caller deadline")
	}
}
