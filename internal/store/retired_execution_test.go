package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRetiredUnsubmittedExecutionFailsOnRestart(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "legacy", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Old preflight owners admitted before recording the parent Workflow name.
	if _, err := s.MarkRunAdmitted(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE runs SET phase='preflight' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.RecoverOwnerState(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Run(t.Context(), run.ID)
	if err != nil || got.Status != model.RunFailed || got.FinishedAt == nil || !strings.Contains(got.Error, "workflow capability grant") {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.ListRecentRuns(t.Context(), model.RunListFilter{RepoID: repo.ID}); err != nil {
		t.Fatal(err)
	}
}
