package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestClaimFIFOAcrossRepositoriesWithinPriority(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	other, err := s.CreateRepository(t.Context(), model.RepositorySpec{Name: "second", UpstreamID: repo.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(repo int64, ref, trigger string) model.Run {
		t.Helper()
		spec := testRunSpec(repo, ref, strings.Repeat("a", 40))
		spec.Trigger = trigger
		r, err := s.EnqueueRun(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		return r.Run
	}
	a := enqueue(repo.ID, "first", "branch")
	pending := enqueue(repo.ID, "second", "branch")
	b := enqueue(other.ID, "first", "branch")
	claimed, err := s.ClaimNextRun(t.Context())
	if err != nil || claimed.ID != a.ID {
		t.Fatal(claimed, err)
	}
	peek, err := s.NextQueuedRun(t.Context())
	if err != nil || peek.ID != pending.ID {
		t.Fatalf("FIFO acceptance order changed: %+v %v", peek, err)
	}
	promotion := enqueue(repo.ID, "promotion/main/one", "promotion")
	for _, want := range []model.Run{promotion, pending, b} {
		got, err := s.ClaimNextRun(t.Context())
		if err != nil || got.ID != want.ID {
			t.Fatalf("claim=%+v want=%s err=%v", got, want.ID, err)
		}
	}
}
