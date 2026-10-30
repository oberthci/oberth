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
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
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

func TestExecutorIdentityFailureIsInfrastructureAndRunRetryApplies(t *testing.T) {
	fixture := newControlFixture(t)
	fixture.jobs.waitErr = fmt.Errorf("observe: %w", &identityObservationFailure{cause: context.DeadlineExceeded})
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
	if failed.Status != model.RunFailed || failed.Phase != "infrastructure" || failed.FailedStep != "" || !strings.Contains(failed.Error, "reason=deadline") {
		t.Fatalf("lost infrastructure cause: %+v", failed)
	}
	if len(fixture.jobs.deleted) != 1 {
		t.Fatal("live Workflow was not deleted before recording infrastructure failure")
	}
	control := fixture.api(t)
	control.runRetries = fixture.store
	value, err := control.CallTool(t.Context(), api.Actor{Identity: "operator@host"}, "run_retry", json.RawMessage(fmt.Sprintf(`{"id":%q}`, failed.ID)))
	if err != nil {
		t.Fatal(err)
	}
	retried := value.(model.Run)
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
				jobs.terminalResult = JobResult{Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit", Error: "assertion failed"}
			}
			err := fixture.scheduler(t, jobs).executeAndCleanup(t.Context(), run)
			current, lookupErr := fixture.store.Run(t.Context(), run.ID)
			if lookupErr != nil {
				t.Fatal(lookupErr)
			}
			if deletionFails {
				if err == nil || current.Status != model.RunRunning {
					t.Fatalf("failed deletion permitted a retryable terminal state: %+v %v", current, err)
				}
			} else if err != nil || current.Phase != "test" || current.FailedStep != "unit" || current.Error != "assertion failed" || len(jobs.deleted) != 0 {
				t.Fatalf("identity observation overrode actual CI failure: %+v %v", current, err)
			}
			if _, err := fixture.store.RetryRun(t.Context(), model.RunRetryRequest{RunID: run.ID, Actor: "operator@host"}); !errors.Is(err, store.ErrInvalidState) {
				t.Fatal("unsafe or actual test failure became retryable", err)
			}
		})
	}
}

func TestExecutorIdentityValidationFailureStaysJob(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/invalid-identity", strings.Repeat("c", 40))
	jobs := &recoveryJobs{waitErr: &identityObservationFailure{cause: errors.New("invalid identity target"), hard: true}, terminalErr: ErrJobNotTerminal}
	if err := fixture.scheduler(t, jobs).executeAndCleanup(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	failed, err := fixture.store.Run(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != model.RunFailed || failed.Phase != "job" || len(jobs.deleted) != 1 {
		t.Fatalf("validation failure was labelled infrastructure or left alive: %+v", failed)
	}
}

func TestExecutorIdentityAdoptedFailureKeepsClassification(t *testing.T) {
	for _, hard := range []bool{false, true} {
		t.Run(fmt.Sprintf("hard=%t", hard), func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			run := fixture.enqueueAndClaim(t, "feature/adopted-identity", strings.Repeat("d", 40))
			fixture.setJobName(t, run.ID, "adopted-identity")
			run.JobName = "adopted-identity"
			jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{waitErr: &identityObservationFailure{cause: context.DeadlineExceeded, hard: hard}, terminalErr: ErrJobNotTerminal}}
			scheduler := fixture.scheduler(t, jobs)
			file, err := scheduler.logs.Create(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			reservation, err := scheduler.admission.adopt(run, periapsis.M, argoworkflow.VMIDemand{})
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
			phase := "infrastructure"
			if hard {
				phase = "job"
			}
			if failed.Status != model.RunFailed || failed.Phase != phase || len(jobs.deleted) != 1 {
				t.Fatalf("adoption overrode identity classification: %+v", failed)
			}
		})
	}
}
