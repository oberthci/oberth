package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestWorkflowMismatchReasonSurvivesRecoveryAndCleanupRearm(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	if _, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/mismatch", strings.Repeat("a", 40))); err != nil {
		t.Fatal(err)
	}
	run, err := s.ClaimNextRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunJobName(t.Context(), run.ID, "wf"); err != nil {
		t.Fatal(err)
	}
	const reason = "Workflow spec mismatch; cleanup pending"
	if _, err := s.db.Exec(`UPDATE runs SET error=?, phase='infrastructure-pending' WHERE id=?`, reason, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverOwnerState(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRunCreatePending(t.Context(), run.ID, "wf"); err != nil {
		t.Fatal(err)
	}
	current, err := s.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunRunning || current.Error != reason {
		t.Fatalf("recovery/rearm lost mismatch: %+v %v", current, err)
	}
	if _, err := s.CancelRun(t.Context(), model.RunCancelRequest{RunID: run.ID, Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingRunCancellations(t.Context())
	if err != nil || len(pending) != 1 {
		t.Fatalf("cancel lost cleanup ownership: %+v %v", pending, err)
	}
	if err := s.CompleteObservedRunCancellation(t.Context(), pending[0]); err != nil {
		t.Fatal(err)
	}
	current, err = s.Run(t.Context(), run.ID)
	if err != nil || !current.Cancelled() || strings.Contains(current.Error, "cleanup pending") {
		t.Fatalf("cancelled settled run still claims pending cleanup: %+v %v", current, err)
	}
}
