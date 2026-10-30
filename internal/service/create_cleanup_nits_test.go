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
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type contractSubmitFailureStore struct{ SchedulerStore }

func (*contractSubmitFailureStore) MarkRunSubmitted(context.Context, string) (model.Run, error) {
	return model.Run{}, errors.New("submission record unavailable")
}

type contractFailureJobs struct {
	*pendingCreateJobs
	deleteErr error
}

func (j *contractFailureJobs) Delete(ctx context.Context, name, id string) error {
	if j.deleteErr != nil {
		return j.deleteErr
	}
	return j.pendingCreateJobs.Delete(ctx, name, id)
}

func TestWorkflowCreateContractCleanupAfterDefiniteFailure(t *testing.T) {
	for _, mode := range []string{"create-rejected", "submission-record-failed", "deletion-unconfirmed"} {
		t.Run(mode, func(t *testing.T) {
			f := newControlFixture(t)
			jobs := &contractFailureJobs{pendingCreateJobs: &pendingCreateJobs{fakeJobs: f.jobs}}
			f.scheduler.jobs = jobs
			if mode == "create-rejected" {
				jobs.createHook = func(context.Context, JobRequest) error { return errors.New("create rejected") }
			} else {
				f.scheduler.store = &contractSubmitFailureStore{SchedulerStore: f.store}
			}
			if mode == "deletion-unconfirmed" {
				jobs.deleteErr = errors.New("delete unavailable")
			}
			source, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "contract-failure", Repository: f.repo, Branch: "feature/contract-failure", SHA: strings.Repeat("a", 40), Actor: "pusher"})
			if err != nil {
				t.Fatal(err)
			}
			err = f.scheduler.ProcessNext(t.Context())
			_, statErr := os.Stat(f.scheduler.createAdmissionPath(source.ID))
			current, lookupErr := f.store.Run(t.Context(), source.ID)
			if lookupErr != nil {
				t.Fatal(lookupErr)
			}
			if mode == "deletion-unconfirmed" {
				if err == nil || statErr != nil || current.Status != model.RunRunning {
					t.Fatalf("uncertain active submission lost its contract: run=%+v err=%v stat=%v", current, err, statErr)
				}
			} else if err != nil || !os.IsNotExist(statErr) || current.Status != model.RunFailed {
				t.Fatalf("terminal failure leaked contract: run=%+v err=%v stat=%v", current, err, statErr)
			}
		})
	}
}

func TestWorkflowCreateContractTerminalSweepRespectsPendingCancellation(t *testing.T) {
	f := newRecoveryFixture(t)
	run := recoveryWaitingRun(t, f, "feature/contract-sweep", "wf-contract", "")
	scheduler := f.scheduler(t, &recoveryJobs{})
	if err := scheduler.recordCreateAdmission(run, run.JobName, periapsis.S, argoworkflow.VMIDemand{}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkRunCreatePending(t.Context(), run.ID, run.JobName); err != nil {
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
	if err := f.store.CompleteRunCancellation(t.Context(), run.ID, run.JobName); err != nil {
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

func TestWorkflowCreateRestoreFailureRegistersAndRetries(t *testing.T) {
	f := newRecoveryFixture(t)
	run := recoveryWaitingRun(t, f, "feature/restore-deferred", "wf-restore", "")
	calls := 0
	finish := false
	jobs := &pendingCreateJobs{fakeJobs: &fakeJobs{}, settle: func(_ context.Context, _, _ string, boundary time.Time) (bool, error) { calls++; return finish, nil }}
	scheduler := f.scheduler(t, jobs)
	now := time.Now()
	scheduler.createNow = func() time.Time { return now }
	if err := scheduler.recordCreateAdmission(run, run.JobName, periapsis.S, argoworkflow.VMIDemand{}); err != nil {
		t.Fatal(err)
	}
	// An existing ungranted reservation makes both normal and quarantine
	// adoption refuse. This used to return without registering the run.
	conflict := scheduler.admission.reserveRun(run)
	scheduler.registerCreateCleanup(run, nil, false)
	if scheduler.pendingCreateCount() != 1 {
		t.Fatal("failed restore dropped its run")
	}
	scheduler.settleOneCreate(t.Context())
	current, err := f.store.Run(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !isPendingCreate(current) || calls != 0 || scheduler.pendingCreateCount() != 1 {
		t.Fatalf("unrestored run lost ownership or issued cleanup: %+v calls=%d", current, calls)
	}
	scheduler.createMu.Lock()
	pending := scheduler.createPending[run.ID]
	attempts := pending.attempts
	next := pending.next
	grant := pending.grant
	scheduler.createMu.Unlock()
	if attempts != 1 || !next.Equal(now.Add(5*time.Second)) || grant != nil {
		t.Fatal("restore failure did not enter bounded backoff")
	}
	scheduler.registerCreateCleanup(current, nil, true)
	if scheduler.createPending[run.ID] != pending {
		t.Fatal("re-registration reset deferred state")
	}
	conflict.cancel()
	now = now.Add(5 * time.Second)
	scheduler.settleOneCreate(t.Context())
	if calls != 1 || scheduler.AdmissionState().Used != 1 {
		t.Fatal("restored run not charged before observation")
	}
	finish = true
	now = now.Add(2 * time.Minute)
	scheduler.settleOneCreate(t.Context())
	current, err = f.store.Run(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != model.RunFailed || scheduler.pendingCreateCount() != 0 || scheduler.AdmissionState().Used != 0 {
		t.Fatalf("restored run did not settle/release: %+v", current)
	}
	if _, err := os.Stat(scheduler.createAdmissionPath(run.ID)); !os.IsNotExist(err) {
		t.Fatal("settled restored run leaked contract", err)
	}
}

func TestWorkflowCreateConcurrentRegistrationHasOneGrant(t *testing.T) {
	f := newRecoveryFixture(t)
	run := recoveryWaitingRun(t, f, "feature/register-once", "wf-once", "")
	scheduler := f.scheduler(t, &recoveryJobs{})
	if err := scheduler.recordCreateAdmission(run, run.JobName, periapsis.S, argoworkflow.VMIDemand{}); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 20 {
		workers.Add(1)
		go func() { defer workers.Done(); scheduler.registerCreateCleanup(run, nil, false) }()
	}
	workers.Wait()
	state := scheduler.AdmissionState()
	if scheduler.pendingCreateCount() != 1 || state.Runs != 1 || state.Used != 1 {
		t.Fatalf("concurrent registration duplicated admission: %+v", state)
	}
}
