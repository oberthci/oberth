package store

import (
	"errors"
	"testing"
	"time"
)

func TestClaimEmptyReturnsStoreNotFound(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	if _, err := s.ClaimNextRun(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing store sentinel: %v", err)
	}
}
