package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRefiredFlatQueueUsesGlobalAdmissionPositions(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	other, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "other", UpstreamID: repo.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(repoID int64, ref, sha string) model.Run {
		t.Helper()
		result, err := s.EnqueueRun(ctx, testRunSpec(repoID, ref, strings.Repeat(sha, 40)))
		if err != nil {
			t.Fatal(err)
		}
		return result.Run
	}
	a := enqueue(repo.ID, "feature/a", "a")
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	b := enqueue(other.ID, "feature/b", "b")
	copy, err := s.RequeueStrandedRun(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := enqueue(repo.ID, "feature/c", "c")
	rows, err := s.ListRecentRuns(ctx, model.RunListFilter{})
	if err != nil || len(rows) < 3 || rows[0].ID != copy.ID || rows[1].ID != b.ID || rows[2].ID != c.ID {
		t.Fatalf("flat FIFO order = %v, %v", runIDs(rows), err)
	}
	for i := 0; i < 3; i++ {
		if rows[i].QueuePosition != int64(i+1) {
			t.Fatalf("queue position %d = %d", i, rows[i].QueuePosition)
		}
	}
	claimed, err := s.ClaimNextRun(ctx)
	if err != nil || claimed.ID != copy.ID {
		t.Fatalf("re-fired claim = %#v, %v", claimed, err)
	}
	assertFilteredPosition := func(want int64) {
		t.Helper()
		rows, err := s.ListRecentRuns(ctx, model.RunListFilter{RepoID: other.ID})
		if err != nil || len(rows) != 1 || rows[0].ID != b.ID || rows[0].QueuePosition != want {
			t.Fatalf("filtered global queue position = %#v, %v; want %d", rows, err, want)
		}
		active, err := s.ListActiveRuns(ctx, model.RunListFilter{RepoID: other.ID})
		if err != nil || len(active) != 1 || active[0].ID != b.ID || active[0].QueuePosition != want {
			t.Fatalf("active global queue position = %#v, %v; want %d", active, err, want)
		}
		position, err := s.QueuedRunPosition(ctx, b.ID)
		if err != nil || position != want {
			t.Fatalf("single-run queue position = %d, %v; want %d", position, err, want)
		}
	}
	assertFilteredPosition(2)
	if _, err := s.MarkRunAdmitted(ctx, copy.ID); err != nil {
		t.Fatal(err)
	}
	assertFilteredPosition(1)
	if _, err := s.MarkRunParked(ctx, copy.ID); err != nil {
		t.Fatal(err)
	}
	assertFilteredPosition(2)
}
