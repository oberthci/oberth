package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestFailedRestartLeavesFlatQueuePositions(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := t.Context()
	a, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/a", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/b", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverOwnerState(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListRecentRuns(ctx, model.RunListFilter{})
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	for _, r := range rows {
		if r.ID == a.ID && (r.Status != model.RunFailed || r.QueuePosition != 0) {
			t.Fatal(r)
		}
		if r.ID == b.ID && (r.Status != model.RunQueued || r.QueuePosition != 1) {
			t.Fatal(r)
		}
	}
}
