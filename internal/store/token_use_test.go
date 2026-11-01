package store

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/testtimeout"
)

func TestTokenUseQueueSurvivesWriterSaturationAndCancelsOnShutdown(t *testing.T) {
	s, uplink := authReaderFixture(t)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	s.RecordTokenCredentialUse(canceled, uplink.TokenCredentialID)
	if s.tokenUses != nil {
		t.Fatal("canceled request started telemetry worker")
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	waits := s.db.Stats().WaitCount
	s.RecordTokenCredentialUse(t.Context(), uplink.TokenCredentialID)
	deadline := time.Now().Add(testtimeout.Start(time.Second))
	for s.db.Stats().WaitCount == waits {
		if time.Now().After(deadline) {
			t.Fatal("telemetry worker did not reach held writer")
		}
		time.Sleep(time.Millisecond)
	}
	// Even more due uses than fit in the queue cannot stall authentication.
	queued := make(chan struct{})
	go func() {
		for range 1024 {
			s.RecordTokenCredentialUse(t.Context(), uplink.TokenCredentialID)
		}
		close(queued)
	}()
	select {
	case <-queued:
	case <-time.After(testtimeout.Start(time.Second)):
		t.Fatal("full telemetry queue blocked a caller")
	}
	if len(s.tokenUses) != tokenUseQueueSize {
		t.Fatalf("queue not saturated: %d", len(s.tokenUses))
	}
	ctx, stop := context.WithTimeout(t.Context(), testtimeout.Start(time.Second))
	defer stop()
	if _, err := s.AuthenticatedUplinkByDigest(ctx, make([]byte, 32)); err != nil {
		t.Fatalf("telemetry occupied authentication readers: %v", err)
	}
	// Close must cancel the worker before waiting for SQLite to close; keep
	// the writer held until that cancellation has actually completed.
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case <-s.tokenUsesDone:
	case <-time.After(testtimeout.Start(time.Second)):
		t.Fatal("telemetry worker did not cancel its writer wait")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testtimeout.Start(time.Second)):
		t.Fatal("store close did not join telemetry worker")
	}
	before := len(s.tokenUses)
	s.RecordTokenCredentialUse(t.Context(), uplink.TokenCredentialID)
	if len(s.tokenUses) != before {
		t.Fatal("closed store accepted telemetry")
	}
}
