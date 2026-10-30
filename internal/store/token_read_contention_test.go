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
