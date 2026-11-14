package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestRetiredWorkflowFailsRunWithoutStoppingOwner(t *testing.T) {
	f := newRecoveryFixture(t)
	run := f.enqueueAndClaim(t, "feature/retired", strings.Repeat("a", 40))
	f.setWorkflowName(t, run.ID, "retired-workflow")
	jobs := &adoptingJobs{adoptErr: fmt.Errorf("%w: %w", ErrAdoptionRefused, argoworkflow.ErrWorkflowCapabilityRequired)}
	s := f.scheduler(t, jobs)
	if err := s.reconcileStrandedRunsWithGate(t.Context(), s.requireMutation, func(model.Run) error { t.Fatal("retired workload adopted"); return nil }); err != nil {
		t.Fatal(err)
	}
	s.settleOneCreate(t.Context())
	got, err := f.store.Run(t.Context(), run.ID)
	if err != nil || got.Status != model.RunInterrupted || got.Phase != model.PhaseCancelled || !strings.Contains(got.Error, "workflow capability grant") || len(jobs.deleted) != 1 {
		t.Fatalf("run=%+v delete=%v err=%v", got, jobs.deleted, err)
	}
	if err := s.reconcileStrandedRunsWithGate(t.Context(), s.requireMutation, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRetiredUnsubmittedRunFailsWithoutExecution(t *testing.T) {
	f := newControlFixture(t)
	run, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "retired-restart", Repository: f.repo, Branch: "feature/retired", SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.MarkRunSubmissionStarted(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecoverOwnerState(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.scheduler.git = &policyTestGit{controlGit: f.git, candidate: []byte("historical execution contract")}
	if err := f.scheduler.ProcessNext(t.Context()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	got, err := f.store.Run(t.Context(), run.ID)
	if err != nil || got.Status != model.RunFailed || got.Error != model.ServerRestartReason || got.WorkflowName != "" {
		t.Fatalf("run=%+v err=%v", got, err)
	}
}

func TestPolicyWaitAuditOutageDoesNotFinishRun(t *testing.T) {
	f := newControlFixture(t)
	f.scheduler.git = &policyTestGit{controlGit: f.git, baselineErr: gitcache.ErrPolicyUpstreamUnavailable}
	blocked := false
	f.scheduler.mutationGate = func(context.Context) error {
		if blocked {
			return errors.New("witness offline")
		}
		return nil
	}
	f.scheduler.policyRetryWait = func(context.Context) error { blocked = true; return nil }
	run, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "audit-policy", Repository: f.repo, Branch: "feature/policy", SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.scheduler.ProcessNext(t.Context()); !errors.Is(err, errMutationBlocked) {
		t.Fatal(err)
	}
	got, err := f.store.Run(t.Context(), run.ID)
	if err != nil || got.Status != model.RunRunning || got.FinishedAt != nil {
		t.Fatalf("audit outage terminalized run: %+v %v", got, err)
	}
}
