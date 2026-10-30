package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testtimeout"
)

func TestReadinessReaderSurvivesWriterAndMappingPoolSaturation(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	upstream, err := s.CreateUpstream(t.Context(), model.UpstreamSpec{Name: "readiness", Kind: "local", BaseURL: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.OpenReadinessReaders(t.Context(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	pool := s.readinessDB
	if err := s.OpenReadinessReaders(t.Context(), 10*time.Second); err != nil || s.readinessDB != pool {
		t.Fatalf("startup is not idempotent: %v", err)
	}
	var held []*sql.Conn
	defer func() {
		for _, conn := range held {
			_ = conn.Close()
		}
	}()
	for range liveReadConnections {
		conn, err := s.readDB.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(t.Context(), `UPDATE upstreams SET name='uncommitted' WHERE id=?`, upstream.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(4*time.Second))
	defer cancel()
	for range 10 {
		got, err := s.ListReadinessUpstreams(ctx)
		if err != nil || len(got) != 1 || got[0].Name != upstream.Name {
			t.Fatalf("reserved sampler blocked or saw uncommitted data: %+v %v", got, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListReadinessUpstreams(ctx)
	if err != nil || len(got) != 1 || got[0].Name != "uncommitted" {
		t.Fatalf("sampler reused an old snapshot: %+v %v", got, err)
	}
}

func TestReadinessReaderReadOnlyBoundedAndClosed(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	if _, err := s.ListReadinessUpstreams(t.Context()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("missing startup reader silently fell back: %v", err)
	}
	if err := s.OpenReadinessReaders(t.Context(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	conn, err := s.readinessDB.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var queryOnly int
	if err := conn.QueryRowContext(t.Context(), `PRAGMA query_only`).Scan(&queryOnly); err != nil || queryOnly != 1 {
		t.Fatalf("query_only=%d: %v", queryOnly, err)
	}
	if _, err := conn.ExecContext(t.Context(), `DELETE FROM upstreams`); err == nil {
		t.Fatal("readiness reader accepted a write")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if _, err := s.ListReadinessUpstreams(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reserved pool was not bounded to one connection: %v", err)
	}
	_ = conn.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.readinessDB.PingContext(t.Context()); err == nil {
		t.Fatal("Store.Close left readiness reader open")
	}
}

func TestReadinessReaderBusyTimeoutCoversBudgetOnEveryConnection(t *testing.T) {
	for _, configured := range []time.Duration{5 * time.Second, 25 * time.Second} {
		t.Run(configured.String(), func(t *testing.T) {
			s, err := OpenAdminClient(t.Context(), filepath.Join(t.TempDir(), "readiness.db"), Options{BusyTimeout: configured})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			if err := s.OpenReadinessReaders(t.Context(), 10*time.Second); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				conn, err := s.readinessDB.Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				var timeout, queryOnly int64
				err = conn.QueryRowContext(t.Context(), "PRAGMA busy_timeout").Scan(&timeout)
				if err != nil || timeout != max(configured, 10*time.Second).Milliseconds() {
					t.Fatalf("readiness busy_timeout=%d: %v", timeout, err)
				}
				if err := conn.QueryRowContext(t.Context(), "PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
					t.Fatalf("readiness query_only=%d: %v", queryOnly, err)
				}
				// Discard the physical connection; the replacement must inherit
				// the timeout from the DSN, rather than a one-time PRAGMA.
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				_ = conn.Close()
			}
			var ordinary int64
			if err := s.readDB.QueryRowContext(t.Context(), "PRAGMA busy_timeout").Scan(&ordinary); err != nil || ordinary != configured.Milliseconds() {
				t.Fatalf("ordinary reader timeout changed: %d, %v", ordinary, err)
			}
		})
	}
}
