package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type identityObservationFailure struct {
	cause error
	hard  bool
}

func (failure *identityObservationFailure) Error() string {
	return "deliver executor identity failed (stage=exec reason=deadline attempts=3)"
}
func (failure *identityObservationFailure) Unwrap() error               { return failure.cause }
func (failure *identityObservationFailure) InfrastructureFailure() bool { return !failure.hard }

func TestExecutorIdentityFailureStopsAsCancelledAndRunRetryApplies(t *testing.T) {
	fixture := newControlFixture(t)
	// Manual run_rerun remains available when the administrator disables automatic recovery.

	fixture.jobs.waitErr = fmt.Errorf("observe: %w", &identityObservationFailure{cause: context.DeadlineExceeded})
	fixture.jobs.stopResult = JobResult{Status: model.RunFailed, Phase: "infrastructure", Error: fixture.jobs.waitErr.Error()}
	source, err := fixture.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "identity-delivery", Repository: fixture.repo, Branch: "feature/identity", SHA: strings.Repeat("a", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	failed, err := fixture.store.Run(t.Context(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != model.RunInterrupted || failed.Phase != model.PhaseCancelled || failed.FailedStep != "" || !strings.Contains(failed.Error, "reason=deadline") {
		t.Fatalf("lost infrastructure cause: %+v", failed)
	}
	if len(fixture.jobs.deleted) != 1 {
		t.Fatal("live Workflow was not stopped before recording cancellation")
	}
	control := fixture.api(t)
	control.runReruns = fixture.store
	control.refs = stubRefResolver{branches: map[string]map[string]string{control.cacheInputFor(t.Context(), fixture.repo): {source.Ref: source.SHA}}}
	value, err := control.CallTool(t.Context(), api.Actor{Identity: "operator@host"}, "run_rerun", json.RawMessage(fmt.Sprintf(`{"id":%q}`, source.ID)))
	if err != nil {
		t.Fatal(err)
	}
	retried, err := fixture.store.Run(t.Context(), value.(api.RunRerunResponse).RunID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != model.RunQueued || retried.ID == source.ID || retried.SHA != source.SHA || retried.Ref != source.Ref {
		t.Fatalf("retry lost source identity: %+v", retried)
	}
}

func TestExecutorIdentityFailureKeepsTerminalResultAndDeletionSafety(t *testing.T) {
	for _, deletionFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("deletionFails=%t", deletionFails), func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			run := fixture.enqueueAndClaim(t, "feature/identity", strings.Repeat("b", 40))
			jobs := &recoveryJobs{waitErr: &identityObservationFailure{cause: context.DeadlineExceeded}}
			if deletionFails {
				jobs.terminalErr, jobs.deleteErr = ErrJobNotTerminal, errors.New("API unavailable")
			} else {
				jobs.terminalResult = JobResult{Status: model.RunFailed, Phase: "test", FailedStage: "test", FailedStep: "unit", Error: "assertion failed"}
			}
			err := fixture.scheduler(t, jobs).executeAndCleanup(t.Context(), run)
			current, lookupErr := fixture.store.Run(t.Context(), run.ID)
			if lookupErr != nil {
				t.Fatal(lookupErr)
			}
			if deletionFails {
				if err != nil || current.Status != model.RunRunning || !isPendingCreate(current) {
					t.Fatalf("failed deletion permitted a retryable terminal state: %+v %v", current, err)
				}
			} else if err != nil || current.Phase != "test" || current.FailedStep != "unit" || current.Error != "assertion failed" || len(jobs.deleted) != 0 {
				t.Fatalf("identity observation overrode actual CI failure: %+v %v", current, err)
			}
			if deletionFails {
				if _, err := fixture.store.RerunRun(t.Context(), model.RunRerunRequest{RunID: run.ID, Actor: "operator@host"}); !errors.Is(err, store.ErrInvalidState) {
					t.Fatal("unsafe deletion became retryable", err)
				}
			}
		})
	}
}

func TestExecutorIdentityValidationFailureStopsAsCancelled(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/invalid-identity", strings.Repeat("c", 40))
	jobs := &recoveryJobs{waitErr: &identityObservationFailure{cause: errors.New("invalid identity target"), hard: true}, terminalErr: ErrJobNotTerminal}
	jobs.stopResult = JobResult{Status: model.RunFailed, Phase: "job", Error: jobs.waitErr.Error()}
	if err := fixture.scheduler(t, jobs).executeAndCleanup(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	failed, err := fixture.store.Run(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != model.RunInterrupted || failed.Phase != model.PhaseCancelled || len(jobs.deleted) != 1 {
		t.Fatalf("validation failure was labelled infrastructure or left alive: %+v", failed)
	}
}

func TestExecutorIdentityAdoptedFailureStopsAsCancelled(t *testing.T) {
	for _, hard := range []bool{false, true} {
		t.Run(fmt.Sprintf("hard=%t", hard), func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			run := fixture.enqueueAndClaim(t, "feature/adopted-identity", strings.Repeat("d", 40))
			fixture.setWorkflowName(t, run.ID, "adopted-identity")
			run.WorkflowName = "adopted-identity"
			jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{waitErr: &identityObservationFailure{cause: context.DeadlineExceeded, hard: hard}, terminalErr: ErrJobNotTerminal}}
			phase := "infrastructure"
			if hard {
				phase = "job"
			}
			jobs.stopResult = JobResult{Status: model.RunFailed, Phase: phase, Error: jobs.waitErr.Error()}
			scheduler := fixture.scheduler(t, jobs)
			file, err := scheduler.logs.Create(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			reservation, err := scheduler.submissions.adopt(run)
			if err != nil {
				t.Fatal(err)
			}
			if err := scheduler.resumeAdoptedRun(t.Context(), run, scheduler.requireMutation, reservation); err != nil {
				t.Fatal(err)
			}
			failed, err := fixture.store.Run(t.Context(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if failed.Status != model.RunInterrupted || failed.Phase != model.PhaseCancelled || len(jobs.deleted) != 1 {
				t.Fatalf("adoption overrode identity classification: %+v", failed)
			}
		})
	}
}
