package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type contractSubmitFailureStore struct{ SchedulerStore }

func (*contractSubmitFailureStore) MarkRunSubmitted(context.Context, string) (model.Run, error) {
	return model.Run{}, errors.New("submission record unavailable")
}

type contractFailureJobs struct {
	*pendingCreateJobs
	deleteErr error
}

func (j *contractFailureJobs) Stop(ctx context.Context, name, id string) error {
	if j.deleteErr != nil {
		return j.deleteErr
	}
	return j.pendingCreateJobs.Stop(ctx, name, id)
}

func TestWorkflowCreateContractCleanupAfterDefiniteFailure(t *testing.T) {
	for _, mode := range []string{"create-rejected", "submission-record-failed", "deletion-unconfirmed"} {
		t.Run(mode, func(t *testing.T) {
			f := newControlFixture(t)
			jobs := &contractFailureJobs{pendingCreateJobs: &pendingCreateJobs{fakeJobs: f.jobs}}
			f.scheduler.jobs = jobs
			if mode == "create-rejected" {
				jobs.createHook = func(context.Context, WorkflowRequest) error { return errors.New("create rejected") }
			} else {
				f.scheduler.store = &contractSubmitFailureStore{SchedulerStore: f.store}
			}
			if mode == "deletion-unconfirmed" {
				jobs.deleteErr = errors.New("delete unavailable")
			}
			source, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: "contract-failure", Repository: f.repo, Branch: "feature/contract-failure", SHA: strings.Repeat("a", 40), Actor: "pusher"})
			if err != nil {
				t.Fatal(err)
			}
			err = f.scheduler.ProcessNext(t.Context())
			if mode == "submission-record-failed" {
				f.scheduler.settleOneCreate(t.Context())
			}
			_, statErr := os.Stat(f.scheduler.createAdmissionPath(source.ID))
			current, lookupErr := f.store.Run(t.Context(), source.ID)
			if lookupErr != nil {
				t.Fatal(lookupErr)
			}
			if mode == "deletion-unconfirmed" {
				if err != nil || statErr != nil || current.Status != model.RunRunning || !isPendingCreate(current) || !f.scheduler.ownsCreateCleanup(source.ID) {
					t.Fatalf("uncertain active submission lost its contract: run=%+v err=%v stat=%v", current, err, statErr)
				}
			} else {
				want := model.RunFailed
				if mode == "submission-record-failed" {
					want = model.RunInterrupted
				}
				if err != nil || !os.IsNotExist(statErr) || current.Status != want {
					t.Fatalf("terminal failure leaked contract: run=%+v err=%v stat=%v", current, err, statErr)
				}
			}
		})
	}
}

func TestWorkflowCreateContractTerminalSweepRespectsPendingCancellation(t *testing.T) {
	f := newRecoveryFixture(t)
	run := recoveryWaitingRun(t, f, "feature/contract-sweep", "wf-contract", "")
	scheduler := f.scheduler(t, &recoveryJobs{})
	if err := scheduler.recordWorkflowCreateBinding(run, run.WorkflowName); err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkRunCreatePending(t.Context(), run.ID, run.WorkflowName); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CancelRun(t.Context(), model.RunCancelRequest{RunID: run.ID, Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.recoverOwnedWorkspaces(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scheduler.createAdmissionPath(run.ID)); err != nil {
		t.Fatal("pending cancellation lost recovery metadata", err)
	}
	if _, err := f.store.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunFailed, Phase: model.PhaseCancelled, Error: "stopped by operator"}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CompleteRunCancellation(t.Context(), run.ID, run.WorkflowName); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.recoverOwnedWorkspaces(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scheduler.createAdmissionPath(run.ID)); !os.IsNotExist(err) {
		t.Fatal("terminal startup sweep leaked contract", err)
	}
}

func TestWorkflowCreateContractRemovalFailureIsNonFatal(t *testing.T) {
	f := newRecoveryFixture(t)
	run := recoveryWaitingRun(t, f, "feature/contract-unlink", "wf-unlink", "")
	scheduler := f.scheduler(t, &recoveryJobs{})
	target := scheduler.createAdmissionPath(run.ID)
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "unexpected"), []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunFailed, Phase: "infrastructure"}); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.workspaces.cleanupRun(t.Context(), run.ID); err != nil {
		t.Fatal("inert contract removal error became fatal", err)
	}
}

func TestWorkflowCreateConcurrentRegistrationHasOneGrant(t *testing.T) {
	f := newRecoveryFixture(t)
	run := recoveryWaitingRun(t, f, "feature/register-once", "wf-once", "")
	scheduler := f.scheduler(t, &recoveryJobs{})
	if err := scheduler.recordWorkflowCreateBinding(run, run.WorkflowName); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 20 {
		workers.Add(1)
		go func() { defer workers.Done(); scheduler.registerCreateCleanup(run, false) }()
	}
	workers.Wait()
	state := scheduler.submissionStateForTest()
	if scheduler.pendingCreateCount() != 1 || state.Runs != 1 {
		t.Fatalf("concurrent registration duplicated admission: %+v", state)
	}
}

func (j *contractFailureJobs) SettleCreate(ctx context.Context, name, id string, _ time.Time) (bool, error) {
	if err := j.Stop(ctx, name, id); err != nil {
		return false, err
	}
	return true, nil
}

func (j *contractFailureJobs) Terminate(ctx context.Context, name, id string) error {
	return j.Stop(ctx, name, id)
}
