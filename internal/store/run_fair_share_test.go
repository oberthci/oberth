package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestClaimFairShareAcrossRepositoriesWithinPriority(t *testing.T) {
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
	if err != nil || peek.ID != b.ID {
		t.Fatalf("busy repository jumped fair share: %+v %v", peek, err)
	}
	promotion := enqueue(repo.ID, "promotion/main/one", "promotion")
	for _, want := range []model.Run{promotion, b, pending} {
		got, err := s.ClaimNextRun(t.Context())
		if err != nil || got.ID != want.ID {
			t.Fatalf("claim=%+v want=%s err=%v", got, want.ID, err)
		}
	}
}
func TestFairShareLookupsUseBoundedIndexes(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	for query, index := range map[string]string{
		"SELECT count(*) FROM runs active WHERE active.repo_id=? AND active.status='running' AND active.phase!='publishing'": "runs_repo_active_idx",
		"SELECT max(served.started_at) FROM runs served WHERE served.repo_id=? AND served.started_at IS NOT NULL":            "runs_repo_started_idx",
	} {
		rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, 1)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			found = found || strings.Contains(detail, index)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if !found {
			t.Fatalf("fair-share lookup does not use %s", index)
		}
	}
}
