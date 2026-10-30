package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type workflowCreateFailureJobs struct{ *fakeJobs }

func (*workflowCreateFailureJobs) CreateCI(context.Context, JobRequest) error {
	return fmt.Errorf("argojob: create Workflow exhausted: %w", context.DeadlineExceeded)
}
func (jobs *workflowCreateFailureJobs) CreateRelease(ctx context.Context, request JobRequest) error {
	return jobs.CreateCI(ctx, request)
}

func TestWorkflowCreateExhaustionIsInfrastructureAndRunRetryApplies(t *testing.T) {
	fixture := newControlFixture(t)
	fixture.scheduler.jobs = &workflowCreateFailureJobs{fixture.jobs}
	source, err := fixture.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "create-timeout", Repository: fixture.repo, Branch: "feature/create-timeout", SHA: strings.Repeat("a", 40), Actor: "pusher"})
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
	if failed.Status != model.RunFailed || failed.Phase != "infrastructure" || failed.FailedStep != "" || !strings.Contains(failed.Error, context.DeadlineExceeded.Error()) {
		t.Fatalf("create failure became product failure: %+v", failed)
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
	if len(fixture.jobs.createdCI) != 0 || len(fixture.jobs.deleted) != 0 {
		t.Fatal("uncertain create outcome treated as a confirmed Workflow")
	}
}

type uncertainWorkflowCreateError struct{}

func (*uncertainWorkflowCreateError) Error() string         { return "Workflow create outcome uncertain" }
func (*uncertainWorkflowCreateError) AmbiguousCreate() bool { return true }

type pendingCreateJobs struct {
	*fakeJobs
	createHook func(context.Context, JobRequest)
	settle     func(context.Context, string, string) error
}

func (jobs *pendingCreateJobs) CreateCI(ctx context.Context, req JobRequest) error {
	if jobs.createHook != nil {
		jobs.createHook(ctx, req)
	}
	return &uncertainWorkflowCreateError{}
}
func (jobs *pendingCreateJobs) SettleCreate(ctx context.Context, name, id string) error {
	return jobs.settle(ctx, name, id)
}

func TestWorkflowCreatePendingKeepsAdmissionAndRecoversBeforeRetry(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanupFails=%t", cleanupFails), func(t *testing.T) {
			f := newControlFixture(t)
			jobs := &pendingCreateJobs{fakeJobs: f.jobs}
			f.scheduler.jobs = jobs
			settled := 0
			jobs.settle = func(ctx context.Context, name, id string) error {
				settled++
				run, err := f.store.Run(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				pending, err := f.store.PendingRunCancellations(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if run.Status != model.RunRunning || run.Phase != "infrastructure-pending" || name != run.JobName || len(pending) != 1 || pending[0].Reason != "owner_restart" {
					t.Fatalf("cleanup not durably owned: %+v %+v", run, pending)
				}
				if settled == 1 {
					f.scheduler.admission.mu.Lock()
					used := f.scheduler.admission.used
					f.scheduler.admission.mu.Unlock()
					if used == 0 {
						t.Fatal("pending create freed its admission")
					}
					if _, err := f.store.RetryRun(ctx, model.RunRetryRequest{RunID: id, Actor: "operator@host"}); err == nil {
						t.Fatal("pending create allowed run_retry")
					}
				}
				if cleanupFails {
					return context.DeadlineExceeded
				}
				return nil
			}
			source, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "uncertain", Repository: f.repo, Branch: "feature/uncertain", SHA: strings.Repeat("e", 40), Actor: "pusher"})
			if err != nil {
				t.Fatal(err)
			}
			err = f.scheduler.ProcessNext(t.Context())
			if cleanupFails {
				if err == nil {
					t.Fatal("unresolved cleanup succeeded")
				}
				current, lookupErr := f.store.Run(t.Context(), source.ID)
				if lookupErr != nil {
					t.Fatal(lookupErr)
				}
				f.scheduler.admission.mu.Lock()
				used := f.scheduler.admission.used
				f.scheduler.admission.mu.Unlock()
				if current.Status != model.RunRunning || used == 0 {
					t.Fatalf("unresolved create released run/grant: %+v used=%d", current, used)
				}
				if err := f.store.RecoverOwnerState(t.Context()); err != nil {
					t.Fatal(err)
				}
				pending, _ := f.store.PendingRunCancellations(t.Context())
				if len(pending) != 1 {
					t.Fatal("restart retired uncertain cancellation")
				}
				cleanupFails = false
				recovery, err := NewScheduler(SchedulerConfig{Store: f.store, Git: f.scheduler.git, Logs: f.scheduler.logs, Jobs: jobs, Auditor: f.store, WorkspaceRoot: f.scheduler.workspaceRoot})
				if err != nil {
					t.Fatal(err)
				}
				if err := recovery.reconcileStrandedRunsWithGate(t.Context(), recovery.requireMutation, nil); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			failed, err := f.store.Run(t.Context(), source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if failed.Status != model.RunFailed || failed.Phase != "infrastructure" {
				t.Fatalf("cleanup did not settle infrastructure: %+v", failed)
			}
			if pending, err := f.store.PendingRunCancellations(t.Context()); err != nil || len(pending) != 0 {
				t.Fatalf("cleanup left obligation: %+v %v", pending, err)
			}
			if _, err := f.store.RetryRun(t.Context(), model.RunRetryRequest{RunID: source.ID, Actor: "operator@host"}); err != nil {
				t.Fatal("confirmed cleanup not retryable", err)
			}
		})
	}
}

func TestWorkflowCreatePendingRearmsAlreadyCompletedCancellation(t *testing.T) {
	f := newControlFixture(t)
	jobs := &pendingCreateJobs{fakeJobs: f.jobs}
	f.scheduler.jobs = jobs
	jobs.createHook = func(ctx context.Context, req JobRequest) {
		result, err := f.store.CancelRun(ctx, model.RunCancelRequest{RunID: req.Run.ID, Actor: "operator@host"})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.scheduler.AcceptCancellations(ctx, result.Cancellations); err != nil {
			t.Fatal(err)
		}
		pending, _ := f.store.PendingRunCancellations(ctx)
		if len(pending) != 0 {
			t.Fatal("early NotFound not simulated")
		}
	}
	calls := 0
	jobs.settle = func(ctx context.Context, _, id string) error {
		calls++
		run, _ := f.store.Run(ctx, id)
		pending, _ := f.store.PendingRunCancellations(ctx)
		if !run.Cancelled() || len(pending) != 1 || pending[0].Reason != "owner_restart" {
			t.Fatalf("late commit not rearmed: %+v %+v", run, pending)
		}
		return context.DeadlineExceeded
	}
	source, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "cancel-create", Repository: f.repo, Branch: "feature/cancel-create", SHA: strings.Repeat("f", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.scheduler.ProcessNext(t.Context()); err == nil {
		t.Fatal("failed cleanup released owner")
	}
	run, _ := f.store.Run(t.Context(), source.ID)
	f.scheduler.admission.mu.Lock()
	used := f.scheduler.admission.used
	f.scheduler.admission.mu.Unlock()
	if !run.Cancelled() || calls != 1 || used == 0 {
		t.Fatalf("cancellation lost grant/state: %+v calls=%d used=%d", run, calls, used)
	}
}

type pendingRecoveryJobs struct {
	adoptingJobs
	settled int
}

func (jobs *pendingRecoveryJobs) SettleCreate(context.Context, string, string) error {
	jobs.settled++
	return nil
}

func TestWorkflowCreatePendingRecoveryHandlesMultipleAndUnmarkedCrash(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(fmt.Sprintf("marked=%t", marked), func(t *testing.T) {
			f := newRecoveryFixture(t)
			var runs []model.Run
			for i := range 2 {
				run := f.enqueueAndClaim(t, fmt.Sprintf("feature/pending-%d", i), strings.Repeat("a", 40))
				f.setJobName(t, run.ID, fmt.Sprintf("workflow-%d", i))
				if marked {
					if err := f.store.MarkRunCreatePending(t.Context(), run.ID, fmt.Sprintf("workflow-%d", i)); err != nil {
						t.Fatal(err)
					}
				}
				runs = append(runs, run)
			}
			jobs := &pendingRecoveryJobs{adoptingJobs: adoptingJobs{recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal}}}
			scheduler := f.scheduler(t, jobs)
			if err := f.store.RecoverOwnerState(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := scheduler.reconcileStrandedRunsWithGate(t.Context(), scheduler.requireMutation, nil); err != nil {
				t.Fatal(err)
			}
			if jobs.settled != 2 {
				t.Fatalf("startup skipped late-commit observation: %d", jobs.settled)
			}
			for _, run := range runs {
				current, _ := f.store.Run(t.Context(), run.ID)
				if marked {
					if current.Status != model.RunFailed || current.Phase != "infrastructure" {
						t.Fatalf("marked pending not settled: %+v", current)
					}
				} else if !current.Preempted() {
					t.Fatalf("unmarked crash not settled before fallback: %+v", current)
				}
			}
		})
	}
}
