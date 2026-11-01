package store

import (
	"context"
	"testing"
	"time"
)

// Bearer authentication reads fresh identity and scope via WAL while optional
// token-use telemetry waits for the writer in the background.
func TestBearerIdentityReadIsIndependentOfWriter(t *testing.T) {
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
}

func TestTokenUseWritesAtMostOncePerMinuteUnderConcurrentTouches(t *testing.T) {
	s, uplink := authReaderFixture(t)
	now := time.Now().UTC()
	changes := func() int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := changes()
	if _, err := s.touchTokenCredentialAt(t.Context(), uplink.TokenCredentialID, now); err != nil {
		t.Fatal(err)
	}
	if changes()-before != 1 {
		t.Fatal("first touch was not one actual update")
	}
	now = now.Add(59 * time.Second)
	if _, err := s.touchTokenCredentialAt(t.Context(), uplink.TokenCredentialID, now); err != nil {
		t.Fatal(err)
	}
	if changes()-before != 1 {
		t.Fatal("recent token timestamp was rewritten")
	}
	now = now.Add(time.Second)
	done := make(chan error, 12)
	for range 12 {
		go func() {
			_, err := s.touchTokenCredentialAt(t.Context(), uplink.TokenCredentialID, now)
			done <- err
		}()
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
	if changed, err := s.touchTokenCredentialAt(t.Context(), uplink.TokenCredentialID, now.Add(time.Minute)); err != nil || changed {
		t.Fatalf("telemetry updated revoked credential: changed=%t err=%v", changed, err)
	}
}
