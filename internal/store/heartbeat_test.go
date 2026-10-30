package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testtimeout"
)

func TestWriterHeartbeatUsesWriterWithoutChangingDurableState(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	snapshot := func() [][2]int64 {
		rows, err := s.db.QueryContext(t.Context(), `SELECT version, applied_at FROM schema_migrations ORDER BY version`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var values [][2]int64
		for rows.Next() {
			var value [2]int64
			if err := rows.Scan(&value[0], &value[1]); err != nil {
				t.Fatal(err)
			}
			values = append(values, value)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return values
	}
	if err := s.OpenSSHAuthReaders(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := snapshot()
	head, err := s.VerifyAuditChain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := s.WriterHeartbeat(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("heartbeat changed migration metadata")
	}
	after, err := s.VerifyAuditChain(t.Context())
	if err != nil || !reflect.DeepEqual(head, after) {
		t.Fatalf("heartbeat changed audit head: %v", err)
	}
	// Holding the production connection must stall this probe even though WAL
	// readers remain available. A read-only ping would incorrectly succeed.
	conn, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waitCount := s.db.Stats().WaitCount
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() { close(started); done <- s.WriterHeartbeat(ctx) }()
	<-started
	deadline := time.Now().Add(testtimeout.Start(5 * time.Second))
	for s.db.Stats().WaitCount == waitCount {
		if time.Now().After(deadline) {
			t.Fatal("heartbeat never entered writer pool queue")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := s.authDB.ExecContext(t.Context(), `SELECT 1`); err != nil {
		t.Fatal("WAL reader blocked", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("heartbeat did not wait for writer: %v", err)
		}
	case <-time.After(testtimeout.Start(5 * time.Second)):
		t.Fatal("writer heartbeat ignored shutdown")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.WriterHeartbeat(t.Context()); err != nil {
		t.Fatal("writer did not recover", err)
	}
}

func TestWriterHeartbeatCannotSucceedOnReadOnlyOrClosedStore(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	s.readOnly = true
	if !errors.Is(s.WriterHeartbeat(t.Context()), ErrInvalidState) {
		t.Fatal("read-only store advertised a writer")
	}
	s.readOnly = false
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.WriterHeartbeat(t.Context()) == nil {
		t.Fatal("closed writer advertised progress")
	}
}

func TestWriterHeartbeatDoesNotContendWithRCAuditFence(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprintf("local=%t", local), func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now) // real latest migrated schema, not a fake SQL writer
			head := seedAuditMetricRows(t, s, 2, now)
			for range 5 {
				callbacks := 0
				external := func(model.AuditHead, model.AuditAnchor) error {
					callbacks++
					done := make(chan error, 1)
					go func() {
						for range 5 {
							if err := s.WriterHeartbeat(t.Context()); err != nil {
								done <- err
								return
							}
						}
						done <- nil
					}()
					select {
					case err := <-done:
						return err
					case <-time.After(testtimeout.Start(5 * time.Second)):
						return errors.New("heartbeat blocked by external-proof window")
					}
				}
				var err error
				if local {
					_, _, err = s.VerifyLocalAuditMutationState(t.Context(), head, external)
				} else {
					_, _, err = s.VerifyAuditMutationStateUnanchored(t.Context(), nil, nil, external)
				}
				if err != nil || callbacks != 1 {
					t.Fatalf("RC fence refused own writer heartbeats: callbacks=%d, err=%v", callbacks, err)
				}
			}
		})
	}
}
