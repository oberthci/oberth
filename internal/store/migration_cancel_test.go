package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMigrationResumeBeginFailureReturnsWithoutPanic(t *testing.T) {
	// Cancellation before migration must report its cause, with no rollback panic.
	now := time.Now()
	s := testStore(t, &now)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.migrate(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("migration error: %v", err)
	}
}
