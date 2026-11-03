package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// An explicit highest request leads; the aged normal run has joined high at
// its own, earlier, queue position, so it precedes the high run accepted
// after it.
func TestPriorityReviewExplicitHighestBeforeAgedWork(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	if _, err := s.SetQueuePriorityPatterns(t.Context(), "admin", []string{"rc/*"}); err != nil {
		t.Fatal(err)
	}
	normal, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "old", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	high, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "rc/old", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	admin := true
	spec := testRunSpec(repo.ID, "urgent", strings.Repeat("c", 40))
	spec.PriorityRequest = model.PriorityRequest{Priority: model.PriorityHighest, UnscopedAdmin: &admin}
	highest, err := s.EnqueueRun(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	active, err := s.ListActiveRuns(t.Context(), model.RunListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{highest.ID, normal.ID, high.ID} {
		if active[i].ID != want || active[i].QueuePosition != int64(i+1) {
			t.Errorf("position %d: %+v, want %s", i, active[i], want)
		}
		got, err := s.ClaimNextRun(t.Context())
		if err != nil || got.ID != want {
			t.Errorf("claim %d: %+v %v, want %s", i, got, err, want)
		}
	}
}
