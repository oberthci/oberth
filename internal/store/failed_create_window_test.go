package store

import (
	"github.com/oberthci/oberth/internal/model"
	"strings"
	"testing"
	"time"
)

func TestFailedWorkflowCreatesOnlyReturnsRecentFailures(t *testing.T) {
	now := time.Now().Add(-time.Hour)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	var recentID string
	for i := range 2 {
		run, err := s.EnqueueRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("a", 40), Actor: "owner", Trigger: "branch"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.StartRun(t.Context(), run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetRunWorkflowName(t.Context(), run.ID, "workflow-"+run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunInterrupted, Phase: model.PhaseCancelled}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			now = now.Add(time.Hour)
		} else {
			recentID = run.ID
		}
	}
	runs, err := s.FailedWorkflowCreates(t.Context(), now.Add(-time.Minute))
	if err != nil || len(runs) != 1 || runs[0].ID != recentID {
		t.Fatal(runs, err)
	}
}
