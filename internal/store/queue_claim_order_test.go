package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestEveryQueuedListFollowsEffectiveClaimOrder(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := t.Context()
	if _, err := s.SetQueuePriorityPatterns(ctx, "admin", []string{"rc/*"}); err != nil {
		t.Fatal(err)
	}
	enqueue := func(ref string) model.Run {
		t.Helper()
		result, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, ref, strings.Repeat("a", 40)))
		if err != nil {
			t.Fatal(err)
		}
		return result.Run
	}
	highBefore := enqueue("rc/before")
	aged := enqueue("feature/aged")
	now = now.Add(time.Hour)
	normalBefore := enqueue("feature/first")
	highAfter := enqueue("rc/after")
	normalAfter := enqueue("feature/last")
	want := []string{highBefore.ID, aged.ID, highAfter.ID, normalBefore.ID, normalAfter.ID}
	for _, latest := range []bool{false, true} {
		rows, err := s.ListRecentRuns(ctx, model.RunListFilter{LatestPerRef: latest, Limit: 1})
		if err != nil || strings.Join(runIDs(rows), ",") != strings.Join(want, ",") {
			t.Errorf("latest=%v: %v, %v; want claim order %v", latest, runIDs(rows), err, want)
		}
	}
	rows, err := s.ListActiveRuns(ctx, model.RunListFilter{})
	if err != nil || strings.Join(runIDs(rows), ",") != strings.Join(want, ",") {
		t.Errorf("active: %v, %v; want %v", runIDs(rows), err, want)
	}
	for _, id := range want {
		claimed, err := s.ClaimNextRun(ctx)
		if err != nil || claimed.ID != id {
			t.Fatalf("claimed %s, %v; want %s", claimed.ID, err, id)
		}
	}
}
