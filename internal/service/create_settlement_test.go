package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
)

type pendingCreateJobs struct {
	*fakeJobs
	createHook func(context.Context, WorkflowRequest) error
	settle     func(context.Context, string, string, time.Time) (bool, error)
	adoptCalls atomic.Int32
	adoptHook  func(context.Context, model.Run) (bool, error)
}

func (j *pendingCreateJobs) CreateWorkflow(ctx context.Context, req WorkflowRequest) error {
	if j.createHook != nil {
		return j.createHook(ctx, req)
	}
	if req.Run.Ref == "feature/pending" {
		return &uncertainWorkflowCreateError{}
	}
	return j.fakeJobs.CreateWorkflow(ctx, req)
}
func (j *pendingCreateJobs) Adopt(ctx context.Context, run model.Run) (bool, error) {
	j.adoptCalls.Add(1)
	if j.adoptHook != nil {
		return j.adoptHook(ctx, run)
	}
	return false, nil
}
func (*pendingCreateJobs) WorkflowNamespace() string { return "test-argo" }
func (j *pendingCreateJobs) WaitResumed(ctx context.Context, name string, out io.Writer, _ []runprogress.Event, _ int64) (JobResult, error) {
	return j.Wait(ctx, name, out)
}

var _ JobAdopter = (*pendingCreateJobs)(nil)
var _ JobCreateSettler = (*pendingCreateJobs)(nil)

func (j *pendingCreateJobs) SettleCreate(ctx context.Context, name, id string, boundary time.Time) (bool, error) {
	if j.settle == nil {
		return j.fakeJobs.SettleCreate(ctx, name, id, boundary)
	}
	return j.settle(ctx, name, id, boundary)
}

func awaitCreateState(t *testing.T, s *Scheduler, id string, wantAttempts int, wantGone bool) {
	t.Helper()
	timer := time.NewTimer(testStartTimeout(5 * time.Second))
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		s.createMu.Lock()
		pending, exists := s.createPending[id]
		attempts := 0
		if exists {
			attempts = pending.attempts
		}
		s.createMu.Unlock()
		if wantGone && !exists {
			return
		}
		if !wantGone && exists && attempts >= wantAttempts {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("cleanup state not reached: exists=%t attempts=%d wanted=%d gone=%t", exists, attempts, wantAttempts, wantGone)
		case <-tick.C:
		}
	}
}
func startCreateScheduler(t *testing.T, s *Scheduler) (chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("scheduler exited: %v", err)
			}
		case <-time.After(testStartTimeout(5 * time.Second)):
			t.Error("scheduler did not stop")
		}
	})
	return done, cancel
}

func TestWorkflowCreateBrownoutTenMinutesDoesNotStopServerOrOtherRuns(t *testing.T) {
	for _, mode := range []string{"API-down", "foreground-deleting"} {
		t.Run(mode, func(t *testing.T) {
			f := newControlFixture(t, JobResult{Status: model.RunFailed, Phase: "infrastructure"}, JobResult{Status: model.RunFailed, Phase: "infrastructure"}, JobResult{Status: model.RunFailed, Phase: "infrastructure"})
			jobs := &pendingCreateJobs{fakeJobs: f.jobs}
			f.scheduler.jobs = jobs
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			f.scheduler.createNow = func() time.Time { return time.Unix(0, clock.Load()) }
			ticks := make(chan time.Time)
			f.scheduler.createTicks = ticks
			var recovered atomic.Bool
			var calls atomic.Int32
			jobs.settle = func(_ context.Context, name, id string, boundary time.Time) (bool, error) {
				calls.Add(1)
				run, err := f.store.Run(t.Context(), id)
				if err != nil {
					return false, err
				}
				if name != run.WorkflowName || boundary.IsZero() {
					return false, errors.New("lost cleanup identity/boundary")
				}
				if !recovered.Load() {
					if mode == "API-down" {
						return false, context.DeadlineExceeded
					}
					return false, nil
				}
				return f.scheduler.createTime().After(boundary), nil
			}
			done, _ := startCreateScheduler(t, f.scheduler)
			pending, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "pending", Repository: f.repo, Branch: "feature/pending", SHA: strings.Repeat("a", 40), Actor: "pusher"})
			if err != nil {
				t.Fatal(err)
			}
			awaitCreateState(t, f.scheduler, pending.ID, 1, false)
			for step := range 3 {
				if step > 0 {
					clock.Add(int64(5 * time.Minute))
					select {
					case ticks <- f.scheduler.createTime():
					case <-time.After(testStartTimeout(time.Second)):
						t.Fatal("settler stopped")
					}
					awaitCreateState(t, f.scheduler, pending.ID, step+1, false)
				}
				select {
				case err := <-done:
					done <- err
					t.Fatalf("run-local cleanup killed scheduler: %v", err)
				default:
				}
				if !f.scheduler.Recovered() {
					t.Fatal("pending cleanup withdrew startup readiness")
				}
				run, err := f.store.Run(t.Context(), pending.ID)
				if err != nil {
					t.Fatal(err)
				}
				if run.Status != model.RunRunning || run.Phase != "infrastructure-pending" {
					t.Fatalf("pending run released: %+v", run)
				}
				if _, err := f.store.RetryRun(t.Context(), model.RunRetryRequest{RunID: run.ID, Actor: "operator"}); err == nil {
					t.Fatal("pending create allowed run_retry")
				}
				other, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: fmt.Sprintf("other-%d", step), Repository: f.repo, Branch: fmt.Sprintf("feature/other-%d", step), SHA: strings.Repeat("b", 40), Actor: "pusher"})
				if err != nil {
					t.Fatal(err)
				}
				waitForFixtureRunStatus(t, f, other.ID, model.RunFailed)
				state := f.scheduler.submissionStateForTest()
				found := false
				for _, grant := range state.Reservations {
					if grant.RunID == pending.ID {
						found = true
					}
				}
				if !found {
					t.Fatal("pending create lost its grant")
				}
			}
			if calls.Load() != 3 {
				t.Fatalf("unexpected cleanup storm: %d", calls.Load())
			}
			recovered.Store(true)
			clock.Add(int64(5 * time.Minute))
			select {
			case ticks <- f.scheduler.createTime():
			case <-time.After(testStartTimeout(time.Second)):
				t.Fatal("recovery settler stopped")
			}
			waitForFixtureRunStatus(t, f, pending.ID, model.RunFailed)
			awaitCreateState(t, f.scheduler, pending.ID, 0, true)
			if pendingRows, err := f.store.PendingRunCancellations(t.Context()); err != nil || len(pendingRows) != 0 {
				t.Fatalf("settled row still pending: %+v %v", pendingRows, err)
			}
			finished, err := f.store.Run(t.Context(), pending.ID)
			if err != nil || finished.Status != model.RunFailed {
				t.Fatal(finished, err)
			}
			counts, err := f.store.CountRuns(t.Context())
			if err != nil || counts.Total != 4 {
				t.Fatal("cleanup created an automatic attempt", counts, err)
			}

		})
	}
}

func TestWorkflowCreateRestartRestoresGrantsWithoutWaitingForSettlement(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(fmt.Sprintf("marked=%t", marked), func(t *testing.T) {
			f := newControlFixture(t, JobResult{Status: model.RunFailed, Phase: "infrastructure"})
			jobs := &pendingCreateJobs{fakeJobs: f.jobs}
			f.scheduler.jobs = jobs
			f.scheduler.submissions = newSubmissionRegistry()
			var runs []model.Run
			for i := range 2 {
				queued, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: fmt.Sprintf("restart-%d", i), Repository: f.repo, Branch: fmt.Sprintf("feature/restart-%d", i), SHA: strings.Repeat("c", 40), Actor: "pusher"})
				if err != nil {
					t.Fatal(err)
				}
				run, err := f.store.ClaimNextRun(t.Context())
				if err != nil || run.ID != queued.ID {
					t.Fatal(err)
				}
				run, err = f.store.SetRunWorkflowName(t.Context(), run.ID, fmt.Sprintf("wf-%d", i))
				if err != nil {
					t.Fatal(err)
				}
				// A possible Workflow POST always follows durable admission.
				run, err = f.store.MarkRunSubmissionStarted(t.Context(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.scheduler.recordWorkflowCreateBinding(run, run.WorkflowName); err != nil {
					t.Fatal(err)
				}
				if marked {
					if err := f.store.MarkRunCreatePending(t.Context(), run.ID, run.WorkflowName); err != nil {
						t.Fatal(err)
					}
				}
				runs = append(runs, run)
			}
			if err := f.store.RecoverOwnerState(t.Context()); err != nil {
				t.Fatal(err)
			}
			// A blocking API call must not delay startup or dispatch of unrelated work.
			apiEntered := make(chan struct{}, 1)
			jobs.adoptHook = func(ctx context.Context, _ model.Run) (bool, error) {
				if jobs.adoptCalls.Load() <= 2 {
					return false, nil
				}
				select {
				case apiEntered <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return false, ctx.Err()
			}

			startup := make(chan struct{})
			f.scheduler.onStartupRecovered = func() { close(startup) }
			done, _ := startCreateScheduler(t, f.scheduler)
			select {
			case <-startup:
			case err := <-done:
				done <- err
				t.Fatalf("startup failed: %v", err)
			case <-time.After(testStartTimeout(time.Second)):
				t.Fatal("startup waited for pending creates")
			}
			select {
			case <-apiEntered:
			case <-time.After(testStartTimeout(time.Second)):
				t.Fatal("pending worker never started")
			}
			if jobs.adoptCalls.Load() < 3 {
				t.Fatal("background recovery did not observe the Workflow")
			}

			state := f.scheduler.submissionStateForTest()
			if state.Runs != 2 || f.scheduler.pendingCreateCount() != 2 {
				t.Fatalf("restart did not restore exact pending contracts: %+v", state)
			}
			other, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "after-restart", Repository: f.repo, Branch: "feature/available", SHA: strings.Repeat("d", 40), Actor: "pusher"})
			if err != nil {
				t.Fatal(err)
			}
			waitForFixtureRunStatus(t, f, other.ID, model.RunFailed)
			for _, run := range runs {
				current, err := f.store.Run(t.Context(), run.ID)
				if err != nil || current.Status != model.RunRunning {
					t.Fatalf("startup discarded pending run: %+v %v", current, err)
				}
			}
		})
	}
}

func TestWorkflowCreateProcessNextAndCancelNeverSettleInline(t *testing.T) {
	f := newControlFixture(t)
	jobs := &pendingCreateJobs{fakeJobs: f.jobs}
	f.scheduler.jobs = jobs
	calls := 0
	jobs.settle = func(context.Context, string, string, time.Time) (bool, error) {
		calls++
		return false, context.DeadlineExceeded
	}
	source, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "pending-cancel", Repository: f.repo, Branch: "feature/pending", SHA: strings.Repeat("e", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatal("run-local uncertainty reached dispatcher", err)
	}
	if calls != 0 || !f.scheduler.ownsCreateCleanup(source.ID) {
		t.Fatal("ProcessNext settled inline or lost owner")
	}
	// The snapshot survives ordinary workspace removal and cannot be supplied by repo files.
	run, _ := f.store.Run(t.Context(), source.ID)
	saved, err := f.scheduler.readCreateAdmission(run)
	if err != nil {
		t.Fatalf("durable contract missing: %+v %v", saved, err)
	}
	_ = os.RemoveAll(f.scheduler.workspaceRoot + "/" + source.ID)
	if _, err := f.scheduler.readCreateAdmission(run); err != nil {
		t.Fatal("workspace cleanup removed recovery contract", err)
	}
	result, err := f.store.CancelRun(t.Context(), model.RunCancelRequest{RunID: source.ID, Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.scheduler.AcceptCancellations(t.Context(), result.Cancellations); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("cancel waited on Kubernetes cleanup")
	}
	current, _ := f.store.Run(t.Context(), source.ID)
	if !current.Cancelled() || !isPendingCreate(current) {
		t.Fatalf("cancel lost durable pending marker: %+v", current)
	}
}

func TestWorkflowCreateSettlementBackoffIsCapped(t *testing.T) {
	want := []time.Duration{5, 10, 20, 40, 80, 160, 300, 300}
	for i, seconds := range want {
		if got := createSettlementBackoff(i + 1); got != seconds*time.Second {
			t.Fatalf("attempt %d backoff=%s", i+1, got)
		}
	}
}

type deferredPendingStore struct {
	SchedulerStore
	fail bool
}

func (s *deferredPendingStore) MarkRunCreatePending(ctx context.Context, id, name string, reason ...string) error {
	if s.fail {
		return context.DeadlineExceeded
	}
	return s.SchedulerStore.MarkRunCreatePending(ctx, id, name, reason...)
}
func TestWorkflowCreatePendingPersistenceFailureIsRetriedWithoutFatal(t *testing.T) {
	f := newControlFixture(t)
	db := &deferredPendingStore{SchedulerStore: f.store, fail: true}
	f.scheduler.store = db
	jobs := &pendingCreateJobs{fakeJobs: f.jobs, settle: func(context.Context, string, string, time.Time) (bool, error) { return false, context.DeadlineExceeded }}
	f.scheduler.jobs = jobs
	source, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "persist-deferred", Repository: f.repo, Branch: "feature/pending", SHA: strings.Repeat("a", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatal("pending persistence failure reached scheduler", err)
	}
	db.fail = false
	f.scheduler.settleOneCreate(t.Context())
	run, err := f.store.Run(t.Context(), source.ID)
	if err != nil || !isPendingCreate(run) {
		t.Fatalf("background failed to persist pending: %+v %v", run, err)
	}
	pending, err := f.store.PendingRunCancellations(t.Context())
	if err != nil || len(pending) != 1 {
		t.Fatalf("cleanup obligation missing: %+v %v", pending, err)
	}
}

func TestWorkflowCreatePendingPushAndResolvedCancelStayLocal(t *testing.T) {
	for _, mode := range []string{"push", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newControlFixture(t)
			calls := 0
			jobs := &pendingCreateJobs{fakeJobs: f.jobs, settle: func(context.Context, string, string, time.Time) (bool, error) { calls++; return true, nil }}
			f.scheduler.jobs = jobs
			source, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "pending-local", Repository: f.repo, Branch: "feature/pending", SHA: strings.Repeat("b", 40), Actor: "pusher"})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.scheduler.ProcessNext(t.Context()); err != nil {
				t.Fatal(err)
			}
			if mode == "push" {
				if _, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "newer-push", Repository: f.repo, Branch: "feature/pending", SHA: strings.Repeat("c", 40), Actor: "pusher"}); err != nil {
					t.Fatal(err)
				}
			} else {
				result, err := f.store.CancelRun(t.Context(), model.RunCancelRequest{RunID: source.ID, Actor: "operator"})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.scheduler.AcceptCancellations(t.Context(), result.Cancellations); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 0 {
				t.Fatal("push/cancel settled inline or released grant")
			}
			f.scheduler.settleOneCreate(t.Context())
			run, err := f.store.Run(t.Context(), source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (mode == "cancel" && !run.Cancelled()) || isPendingCreate(run) || f.scheduler.pendingCreateCount() != 0 || f.scheduler.submissionStateForTest().Runs != 0 {
				t.Fatalf("settled preemption lost state or still pending: %+v", run)
			}
		})
	}
}

func TestWorkflowCreatePendingStatusDoesNotCallKubernetes(t *testing.T) {
	f := newControlFixture(t)
	jobs := &pendingCreateJobs{fakeJobs: f.jobs}
	f.scheduler.jobs = jobs
	source, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "pending-status", Repository: f.repo, Branch: "feature/pending", SHA: strings.Repeat("d", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.Run(t.Context(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	api := f.api(t)
	api.schedulingObserver = schedulingObserverFunc(func(context.Context, model.Run) (*model.ExecutionObservation, error) {
		t.Fatal("pending status attempted a Kubernetes read")
		return nil, nil
	})
	response, err := api.statusForRun(t.Context(), f.repo, run)
	if err != nil {
		t.Fatal(err)
	}
	// The uncertain create is the running run's reason; the raw phase and
	// scheduling state stay machine-readable.
	if response.DisplayStatus != "running" || response.Run.Phase != "infrastructure-pending" || !strings.HasPrefix(response.DisplayReason, "cancelling by oberth:") || response.Scheduling.State != "infra-pending" {
		t.Fatalf("pending status hidden: %+v", response)
	}
}

func TestTerminalCancellationHandoffDoesNotRearmStop(t *testing.T) {
	f := newControlFixture(t)
	enqueued, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "terminal-handoff", Repository: f.repo, Branch: "feature/terminal", SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.ClaimNextRun(t.Context())
	if err != nil || run.ID != enqueued.ID {
		t.Fatal(run, err)
	}
	if _, err := f.store.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunFailed, Phase: "job", Error: "native terminal verdict"}); err != nil {
		t.Fatal(err)
	}
	f.scheduler.createPending = map[string]*pendingCreate{run.ID: {runID: run.ID}}
	f.scheduler.settleOneCreate(t.Context())
	pending, err := f.store.PendingRunCancellations(t.Context())
	if err != nil || len(pending) != 0 || f.scheduler.pendingCreateCount() != 0 {
		t.Fatalf("terminal handoff rearmed custody: pending=%+v error=%v", pending, err)
	}
}
