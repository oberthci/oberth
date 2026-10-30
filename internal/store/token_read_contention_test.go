package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Bearer authentication has a hidden write dependency even though its identity
// and scope lookup uses WAL readers. Keep this regression as evidence for the
// HTTP authentication budget; it must not be "fixed" by ignoring revocation.
func TestBearerIdentityReadIsIndependentButTokenTouchWaitsForWriter(t *testing.T) {
	s, uplink := authReaderFixture(t)
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	identity, err := s.AuthenticatedUplinkByDigest(ctx, make([]byte, 32))
	if err != nil || identity.TokenCredentialID != uplink.TokenCredentialID {
		t.Fatalf("reader queued behind writer: %+v %v", identity, err)
	}
	touchCtx, cancelTouch := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancelTouch()
	if err := s.TouchTokenCredential(touchCtx, uplink.TokenCredentialID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("token touch did not expose writer wait: %v", err)
	}
}

func TestTokenUseWritesAtMostOncePerMinuteUnderConcurrentTouches(t *testing.T) {
	s, uplink := authReaderFixture(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	changes := func() int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := changes()
	if err := s.TouchTokenCredential(t.Context(), uplink.TokenCredentialID); err != nil {
		t.Fatal(err)
	}
	if changes()-before != 1 {
		t.Fatal("first touch was not one actual update")
	}
	now = now.Add(59 * time.Second)
	if err := s.TouchTokenCredential(t.Context(), uplink.TokenCredentialID); err != nil {
		t.Fatal(err)
	}
	if changes()-before != 1 {
		t.Fatal("recent token timestamp was rewritten")
	}
	now = now.Add(time.Second)
	done := make(chan error, 12)
	for range 12 {
		go func() { done <- s.TouchTokenCredential(t.Context(), uplink.TokenCredentialID) }()
	}
	for range 12 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if changes()-before != 2 {
		t.Fatalf("concurrent due touches wrote more than once: %d", changes()-before)
	}
	if err := s.RevokeTokenCredential(t.Context(), uplink.TokenCredentialID); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchTokenCredential(t.Context(), uplink.TokenCredentialID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("throttle hid revocation: %v", err)
	}
}
