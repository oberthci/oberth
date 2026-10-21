package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestQueuedRunPositionIncludesClaimedUnadmitted(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	first, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "first", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "second", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if position, err := s.QueuedRunPosition(ctx, second.ID); err != nil || position != 2 {
		t.Fatalf("position=%d err=%v", position, err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if position, err := s.QueuedRunPosition(ctx, second.ID); err != nil || position != 2 {
		t.Fatalf("position=%d err=%v", position, err)
	}
	if position, err := s.QueuedRunPosition(ctx, first.ID); err != nil || position != 1 {
		t.Fatalf("claimed position=%d err=%v", position, err)
	}
	if _, err := s.MarkRunAdmitted(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if position, err := s.QueuedRunPosition(ctx, second.ID); err != nil || position != 1 {
		t.Fatalf("after grant position=%d err=%v", position, err)
	}
	if _, err := s.QueuedRunPosition(ctx, first.ID); err == nil {
		t.Fatal("admitted run counted as queued")
	}
	if _, err := s.QueuedRunPosition(ctx, "absent"); err == nil {
		t.Fatal("absent run counted as queued")
	}
}
