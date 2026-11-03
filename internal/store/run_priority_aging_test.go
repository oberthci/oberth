package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// TestQueuePriorityAgingMovesOneClassAndKeepsFIFO mixes an explicit highest
// bump, high runs accepted before and after an aged normal run, and a fresh
// normal run. Aging raises the normal run into high only, at its own queue
// position: the bump leads, then high FIFO with the aged run in its place,
// then the fresh normal run.
func TestQueuePriorityAgingMovesOneClassAndKeepsFIFO(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	if _, err := s.SetQueuePriorityPatterns(ctx, "admin", []string{"rc/*"}); err != nil {
		t.Fatal(err)
	}
	enqueue := func(ref, sha string) model.Run {
		t.Helper()
		result, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, ref, strings.Repeat(sha, 40)))
		if err != nil {
			t.Fatal(err)
		}
		return result.Run
	}
	highBefore := enqueue("rc/before", "a")
	now = now.Add(time.Minute)
	aged := enqueue("feature/aged", "b")
	now = now.Add(time.Hour)
	highAfter := enqueue("rc/after", "c")
	fresh := enqueue("feature/fresh", "d")
	bumped := enqueue("feature/blocker", "e")
	if _, err := s.BumpRunPriority(ctx, "admin", bumped.ID, model.PriorityHighest, "release blocker"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)

	shown, err := s.Run(ctx, aged.ID)
	if err != nil {
		t.Fatal(err)
	}
	if priority, reason := shown.EffectivePriority(now); priority != model.PriorityHigh || !strings.Contains(reason, "aged") || strings.Contains(reason, "highest") {
		t.Fatalf("aged normal run = %s / %q, want high with an aged reason", priority, reason)
	}
	shown, err = s.Run(ctx, highBefore.ID)
	if err != nil {
		t.Fatal(err)
	}
	if priority, reason := shown.EffectivePriority(now); priority != model.PriorityHigh || !strings.Contains(reason, "rc/*") {
		t.Fatalf("waiting high run = %s / %q, want its own high class, never highest", priority, reason)
	}

	want := []string{bumped.ID, highBefore.ID, aged.ID, highAfter.ID, fresh.ID}
	flat, err := s.ListRecentRuns(ctx, model.RunListFilter{})
	if err != nil || len(flat) != len(want) {
		t.Fatalf("run list = %v, %v", runIDs(flat), err)
	}
	for i, id := range want {
		if row := flat[len(flat)-1-i]; row.ID != id || row.QueuePosition != int64(i+1) {
			t.Fatalf("listed queue bottom-up at %d = %s (position %d), want %s", i, row.Ref, row.QueuePosition, id)
		}
	}
	for _, id := range want {
		claimed, err := s.ClaimNextRun(ctx)
		if err != nil || claimed.ID != id {
			t.Fatalf("admission order: claimed %+v, want %s (%v)", claimed, id, err)
		}
	}
}

// TestQueuePriorityAgingBoundsStarvationBehindHighStream keeps a normal run
// behind a continuous stream of high runs for an hour. Once it ages it joins
// high at its own, earliest, queue position, so it is admitted before every
// high run accepted after it, however many keep arriving. Only highest work
// (here an explicit bump) still precedes it.
func TestQueuePriorityAgingBoundsStarvationBehindHighStream(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	if _, err := s.SetQueuePriorityPatterns(ctx, "admin", []string{"rc/*"}); err != nil {
		t.Fatal(err)
	}
	starved, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/starved", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	stream := func(name string) model.Run {
		t.Helper()
		result, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "rc/"+name, strings.Repeat("b", 40)))
		if err != nil {
			t.Fatal(err)
		}
		return result.Run
	}
	// For the first hour the high stream is admitted first, FIFO within it.
	var firstHigh model.Run
	for minute := 10; minute < 60; minute += 10 {
		now = now.Add(10 * time.Minute)
		high := stream(time.Duration(minute).String())
		if firstHigh.ID == "" {
			firstHigh = high
		}
		if next, err := s.NextQueuedRun(ctx); err != nil || next.ID != firstHigh.ID {
			t.Fatalf("at %d minutes the next run = %+v, want the oldest high run (%v)", minute, next, err)
		}
	}
	now = now.Add(10 * time.Minute)
	// At the hour the normal run has aged into high at its own position:
	// it leads the high class even as more high runs keep arriving.
	for _, name := range []string{"late-1", "late-2"} {
		stream(name)
		if next, err := s.NextQueuedRun(ctx); err != nil || next.ID != starved.ID {
			t.Fatalf("aged run lost to %s: next = %+v, %v", name, next, err)
		}
	}
	if priority, reason := mustRun(t, s, starved.ID).EffectivePriority(now); priority != model.PriorityHigh || !strings.Contains(reason, "aged") {
		t.Fatalf("aged display = %s / %q", priority, reason)
	}
	// Highest work still leads, so the bound is 60 minutes plus whatever
	// highest work an administrator or release puts ahead.
	bumped := stream("blocker")
	if _, err := s.BumpRunPriority(ctx, "admin", bumped.ID, model.PriorityHighest, "release blocker"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{bumped.ID, starved.ID} {
		claimed, err := s.ClaimNextRun(ctx)
		if err != nil || claimed.ID != id {
			t.Fatalf("claimed %+v, want %s (%v)", claimed, id, err)
		}
	}
}

func mustRun(t *testing.T, s *Store, id string) model.Run {
	t.Helper()
	run, err := s.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}
