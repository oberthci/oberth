package service

import (
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestRetiredWorkflowFailsRunWithoutStoppingOwner(t *testing.T) {
	f := newRecoveryFixture(t)
	run := f.enqueueAndClaim(t, "feature/retired", strings.Repeat("a", 40))
	f.setWorkflowName(t, run.ID, "retired-workflow")
	jobs := &adoptingJobs{adoptErr: fmt.Errorf("%w: %w", ErrAdoptionRefused, argoworkflow.ErrWorkflowCapabilityRequired)}
	s := f.scheduler(t, jobs)
	if err := s.reconcileStrandedRunsWithGate(t.Context(), s.requireMutation, func(model.Run, pipeline.Size) error { t.Fatal("retired workload adopted"); return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Run(t.Context(), run.ID)
	if err != nil || got.Status != model.RunFailed || !strings.Contains(got.Error, "workflow capability grant") || len(jobs.deleted) != 1 {
		t.Fatalf("run=%+v delete=%v err=%v", got, jobs.deleted, err)
	}
	if err := s.reconcileStrandedRunsWithGate(t.Context(), s.requireMutation, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRetiredUnsubmittedRunFailsAfterRequeue(t *testing.T) {
	f := newControlFixture(t)
	run, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "retired-restart", Repository: f.repo, Branch: "feature/retired", SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.MarkRunAdmitted(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecoverOwnerState(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.scheduler.git = &policyTestGit{controlGit: f.git, candidate: []byte("historical execution contract")}
	if err := f.scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Run(t.Context(), run.ID)
	if err != nil || got.Status != model.RunFailed || !strings.Contains(got.Error, "workflow capability grant") || got.WorkflowName != "" {
		t.Fatalf("run=%+v err=%v", got, err)
	}
}
