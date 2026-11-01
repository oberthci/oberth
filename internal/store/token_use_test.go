package store

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
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
		for i := range 1024 {
			s.RecordTokenCredentialUse(t.Context(), fmt.Sprintf("queued-%d", i))
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
	if s.tokenUseQueueDrops.Load() != 1024-tokenUseQueueSize {
		t.Fatalf("queue drops not counted: %d", s.tokenUseQueueDrops.Load())
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

func waitTokenUse(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(testtimeout.Start(2 * time.Second))
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("token-use worker did not reach expected state")
		}
		time.Sleep(time.Millisecond)
	}
}

func tokenUsePending(s *Store) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pendingTokenUses)
}

func TestTokenUseDedupePreservesColdTokenAndUseTime(t *testing.T) {
	s, hot := authReaderFixture(t)
	digest := make([]byte, 32)
	digest[0] = 1
	cold, err := s.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: "cold", Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertUplink(t.Context(), model.UplinkSpec{Identity: "cold", Fingerprint: "SHA256:cold", TokenCredentialID: cold.ID, AuthActor: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateTokenCredential(t.Context(), cold.ID); err != nil {
		t.Fatal(err)
	}
	usedAt := time.Now().UTC()
	var clock atomic.Int64
	clock.Store(usedAt.UnixNano())
	s.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	waits := s.db.Stats().WaitCount
	s.RecordTokenCredentialUse(t.Context(), hot.TokenCredentialID)
	waitTokenUse(t, func() bool { return s.db.Stats().WaitCount > waits })
	for range 1024 {
		s.RecordTokenCredentialUse(t.Context(), hot.TokenCredentialID)
	}
	s.RecordTokenCredentialUse(t.Context(), cold.ID)
	if len(s.tokenUses) != 1 || tokenUsePending(s) != 2 || s.tokenUseQueueDrops.Load() != 0 {
		t.Fatalf("hot token crowded out cold use: queued=%d pending=%d drops=%d", len(s.tokenUses), tokenUsePending(s), s.tokenUseQueueDrops.Load())
	}
	// Move the worker clock after both uses: the stored values must retain
	// observed use time despite the writer stall, rather than commit time.
	clock.Store(usedAt.Add(90 * time.Second).UnixNano())
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	waitTokenUse(t, func() bool { return tokenUsePending(s) == 0 })
	for _, id := range []string{hot.TokenCredentialID, cold.ID} {
		var at int64
		if err := s.reader().QueryRowContext(t.Context(), `SELECT last_used_at FROM token_credentials WHERE id=?`, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if at != usedAt.UnixNano() {
			t.Fatalf("worker recorded completion time instead of use time: %d want %d", at, usedAt.UnixNano())
		}
	}
	if s.tokenUseAttempts.Load() != 2 {
		t.Fatalf("duplicates reached writer: attempts=%d", s.tokenUseAttempts.Load())
	}
}

func TestTokenUseFailuresBackOffAndReportMetrics(t *testing.T) {
	s, uplink := authReaderFixture(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	s.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	if _, err := s.db.ExecContext(t.Context(), `CREATE TRIGGER fail_token_use BEFORE UPDATE OF last_used_at ON token_credentials BEGIN SELECT RAISE(ABORT, 'test telemetry failure'); END`); err != nil {
		t.Fatal(err)
	}
	s.RecordTokenCredentialUse(t.Context(), uplink.TokenCredentialID)
	waitTokenUse(t, func() bool { return s.tokenUseFailures.Load() == 1 && tokenUsePending(s) == 0 })
	for range 2000 {
		s.RecordTokenCredentialUse(t.Context(), uplink.TokenCredentialID)
	}
	if s.tokenUseAttempts.Load() != 1 || s.tokenUseCooldownDrops.Load() != 2000 {
		t.Fatalf("failed touch retried at request rate: attempts=%d cooldown=%d", s.tokenUseAttempts.Load(), s.tokenUseCooldownDrops.Load())
	}
	clock.Add(int64(tokenUseFailureCooldown))
	s.RecordTokenCredentialUse(t.Context(), uplink.TokenCredentialID)
	waitTokenUse(t, func() bool { return s.tokenUseFailures.Load() == 2 && tokenUsePending(s) == 0 })
	var metrics bytes.Buffer
	s.WriteTokenUseMetrics(&metrics)
	for _, want := range []string{"oberth_token_use_attempts_total 2\n", "oberth_token_use_failures_total 2\n", "oberth_token_use_cooldown_drops_total 2000\n", "oberth_token_use_queue_drops_total 0\n"} {
		if !strings.Contains(metrics.String(), want) {
			t.Fatalf("missing metric %q in %s", want, metrics.String())
		}
	}
	if _, err := s.db.ExecContext(t.Context(), `DROP TRIGGER fail_token_use`); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(tokenUseFailureCooldown))
	s.RecordTokenCredentialUse(t.Context(), uplink.TokenCredentialID)
	waitTokenUse(t, func() bool { return tokenUsePending(s) == 0 })
	got, err := s.TokenCredentialByDigest(t.Context(), make([]byte, 32))
	if err != nil || got.LastUsedAt == nil || got.LastUsedAt.UnixNano() != clock.Load() {
		t.Fatalf("telemetry did not recover after cooldown: %+v %v", got, err)
	}
}

func TestTokenUseNoOpsDoNotAcquireAuthenticationReaders(t *testing.T) {
	for _, state := range []string{"recent", "revoked", "unknown"} {
		t.Run(state, func(t *testing.T) {
			s, uplink := authReaderFixture(t)
			id := uplink.TokenCredentialID
			if state == "recent" {
				if err := s.TouchTokenCredential(t.Context(), id); err != nil {
					t.Fatal(err)
				}
			} else if state == "revoked" {
				if err := s.RevokeTokenCredential(t.Context(), id); err != nil {
					t.Fatal(err)
				}
			} else {
				id = "unknown"
			}
			before, err := s.TokenCredentialByDigest(t.Context(), make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			release := holdProtocolAuthReads(t, s, s.bearerAuthSlots)
			defer release()
			s.RecordTokenCredentialUse(t.Context(), id)
			waitTokenUse(t, func() bool { return s.tokenUseAttempts.Load() == 1 && tokenUsePending(s) == 0 })
			if s.tokenUseFailures.Load() != 0 {
				t.Fatal("no-op telemetry treated as a failure")
			}
			release()
			after, err := s.TokenCredentialByDigest(t.Context(), make([]byte, 32))
			if err != nil || !equalNullableTime(before.LastUsedAt, after.LastUsedAt) || !equalNullableTime(before.RevokedAt, after.RevokedAt) {
				t.Fatalf("no-op changed credential: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}

func equalNullableTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
