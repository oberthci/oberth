package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testtimeout"
)

func TestAuditFenceSucceedsUnderContinuousTokenTouches(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	credential, err := s.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: "poller", Digest: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertUplink(t.Context(), model.UplinkSpec{Identity: "poller", Fingerprint: "SHA256:poller", TokenCredentialID: credential.ID, AuthActor: "poller"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateTokenCredential(t.Context(), credential.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(5*time.Second))
	defer cancel()
	touches := make(chan struct{}, 16)
	done := make(chan error, 1)
	var commits atomic.Int32
	start := make(chan struct{})
	go func() {
		select {
		case <-start:
		case <-ctx.Done():
			done <- nil
			return
		}
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
				if err := s.TouchTokenCredential(ctx, credential.ID); err != nil {
					if ctx.Err() != nil {
						done <- nil
					} else {
						done <- err
					}
					return
				}
				commits.Add(1)
				select {
				case touches <- struct{}{}:
				default:
				}
			}
		}
	}()
	calls := 0
	verifies := 0
	_, _, err = s.verifyAuditWithFence(ctx, func(tx *sql.Tx) (model.AuditHead, model.AuditAnchor, error) {
		verifies++
		head, anchor, _, err := s.verifyAuditMutationState(ctx, tx, nil, nil, false)
		return head, anchor, err
	}, func(model.AuditHead, model.AuditAnchor) error {
		calls++
		close(start)
		// At least ten real token-touch commits during external I/O. The writer
		// keeps ticking until the gate returns, including local re-verification.
		for range 10 {
			select {
			case <-touches:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	cancel()
	if writeErr := <-done; writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil || calls != 1 || verifies < 2 || commits.Load() < 10 {
		t.Fatalf("gate=%v, external=%d, snapshots=%d, token commits=%d", err, calls, verifies, commits.Load())
	}
	t.Logf("gate succeeded with %d continuous token-touch commits, %d snapshots, %d external call", commits.Load(), verifies, calls)
}

func TestAuditFenceBoundsLocalReverificationWithoutRepeatingExternalIO(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	if _, err := s.db.ExecContext(t.Context(), `CREATE TABLE gate_progress(value INTEGER); INSERT INTO gate_progress VALUES(0)`); err != nil {
		t.Fatal(err)
	}
	snapshots, external := 0, 0
	_, _, err := s.verifyAuditWithFence(t.Context(), func(tx *sql.Tx) (model.AuditHead, model.AuditAnchor, error) {
		snapshots++
		head, anchor, _, err := s.verifyAuditMutationState(t.Context(), tx, nil, nil, false)
		if err != nil {
			return head, anchor, err
		}
		// Force a benign commit into every verification window to exhaust the
		// bound deterministically, rather than relying on goroutine scheduling.
		_, err = s.db.ExecContext(t.Context(), `UPDATE gate_progress SET value=value+1`)
		return head, anchor, err
	}, func(model.AuditHead, model.AuditAnchor) error { external++; return nil })
	if !errors.Is(err, ErrAuditStateChanged) || !strings.Contains(err.Error(), "exhausted") || snapshots != 3 || external != 1 {
		t.Fatalf("err=%v, snapshots=%d, external=%d", err, snapshots, external)
	}
}

func TestLiveReadersUseStartupTimeoutAndAvoidPrimaryConnection(t *testing.T) {
	for _, timeout := range []time.Duration{0, 1234 * time.Millisecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			s, err := Open(t.Context(), t.TempDir()+"/readers.sqlite", Options{BusyTimeout: timeout})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			tx, err := s.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			// Primary is held: a reader must use cached startup metadata.
			ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(time.Second))
			defer cancel()
			readers, err := s.openLiveReaders(ctx, 4)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = readers.Close() }()
			want := timeout
			if want == 0 {
				want = defaultBusyTimeout
			}
			var conns []*sql.Conn
			defer func() {
				for _, conn := range conns {
					_ = conn.Close()
				}
			}()
			for range 4 {
				conn, err := readers.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				conns = append(conns, conn)
				var busy, queryOnly int64
				if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
					t.Fatal(err)
				}
				if err := conn.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&queryOnly); err != nil {
					t.Fatal(err)
				}
				if busy != want.Milliseconds() || queryOnly != 1 {
					t.Fatalf("busy=%d query_only=%d", busy, queryOnly)
				}
				if _, err := conn.ExecContext(ctx, `CREATE TABLE forbidden_reader_write(id INTEGER)`); err == nil {
					t.Fatal("read-only connection permitted a write")
				}
			}
		})
	}
}
