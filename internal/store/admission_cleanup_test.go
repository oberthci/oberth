package store

import (
	"testing"
	"time"
)

func TestAdmissionDeadlineCleanupReasonSurvivesRestartAndRearm(t *testing.T) {
	for _, reason := range []string{"Executor identity readiness deadline exceeded after one issuance retry; cleanup pending", "Resource admission expired: no scheduled pod or progress; cleanup pending"} {
		t.Run(reason, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			if _, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "stalled", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")); err != nil {
				t.Fatal(err)
			}
			run, err := s.ClaimNextRun(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.SetRunWorkflowName(t.Context(), run.ID, "wf"); err != nil {
				t.Fatal(err)
			}
			if err = s.MarkRunCreatePending(t.Context(), run.ID, "wf", reason); err != nil {
				t.Fatal(err)
			}
			if err = s.RecoverOwnerState(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err = s.MarkRunCreatePending(t.Context(), run.ID, "wf"); err != nil {
				t.Fatal(err)
			}
			current, err := s.Run(t.Context(), run.ID)
			if err != nil || current.Error != reason || current.Phase != "infrastructure-pending" {
				t.Fatalf("deadline cause was lost: %+v %v", current, err)
			}
		})
	}
}
