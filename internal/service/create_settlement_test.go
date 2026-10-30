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
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type pendingCreateJobs struct {
	*fakeJobs
	createHook func(context.Context, JobRequest) error
	settle     func(context.Context, string, string, time.Time) (bool, error)
	adoptCalls atomic.Int32
}

func (j *pendingCreateJobs) CreateCI(ctx context.Context, req JobRequest) error {
	if j.createHook != nil {
		return j.createHook(ctx, req)
	}
	if req.Run.Ref == "feature/pending" {
		return &uncertainWorkflowCreateError{}
	}
	return j.fakeJobs.CreateCI(ctx, req)
}
func (j *pendingCreateJobs) Adopt(context.Context, model.Run) (periapsis.Size, bool, error) {
	j.adoptCalls.Add(1)
	return periapsis.M, false, nil
}
func (*pendingCreateJobs) JobNamespace() string { return "test-argo" }
func (j *pendingCreateJobs) WaitResumed(ctx context.Context, name string, out io.Writer, _ []runprogress.Event, _ int64) (JobResult, error) {
	return j.Wait(ctx, name, out)
}

var _ JobAdopter = (*pendingCreateJobs)(nil)
var _ JobCreateSettler = (*pendingCreateJobs)(nil)

func (j *pendingCreateJobs) SettleCreate(ctx context.Context, name, id string, boundary time.Time) (bool, error) {
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
				if name != run.JobName || boundary.IsZero() {
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
				state := f.scheduler.AdmissionState()
				found := false
				for _, grant := range state.Reservations {
					if grant.RunID == pending.ID {
						found = true
						if grant.Weight != 2 || grant.Size != "M" {
							t.Fatalf("pending grant changed: %+v", grant)
						}
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
			if _, err := f.store.RetryRun(t.Context(), model.RunRetryRequest{RunID: pending.ID, Actor: "operator"}); err != nil {
				t.Fatal("settled infrastructure not retryable", err)
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
				run, err = f.store.SetRunJobName(t.Context(), run.ID, fmt.Sprintf("wf-%d", i))
				if err != nil {
					t.Fatal(err)
				}
				demand := argoworkflow.VMIDemand{}
				if i == 0 {
					demand = argoworkflow.VMIDemand{VMs: 1, Cores: 1, MemoryBytes: 1 << 30}
				}
				if err := f.scheduler.recordCreateAdmission(run, run.JobName, periapsis.S, demand); err != nil {
					t.Fatal(err)
				}
				if marked {
					if err := f.store.MarkRunCreatePending(t.Context(), run.ID, run.JobName); err != nil {
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
			jobs.settle = func(ctx context.Context, _, _ string, _ time.Time) (bool, error) {
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
			wantAdopts := int32(0)
			if !marked {
				wantAdopts = 2
			}
			if jobs.adoptCalls.Load() != wantAdopts {
				t.Fatalf("unacknowledged recovery bypassed adoption: %d", jobs.adoptCalls.Load())
			}
			state := f.scheduler.AdmissionState()
			if state.Used != 2 || state.Runs != 2 || f.scheduler.pendingCreateCount() != 2 {
				t.Fatalf("restart did not restore exact pending contracts: %+v", state)
			}
			f.scheduler.admission.mu.Lock()
			demand := f.scheduler.admission.kubeVirtUsedLocked(f.repo.ID)
			f.scheduler.admission.mu.Unlock()
			if demand.VMs != 1 || demand.Cores != 1 || demand.MemoryBytes != 1<<30 {
				t.Fatalf("restart lost VMI demand: %+v", demand)
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
	if err != nil || saved.Size != periapsis.M {
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
	if f.scheduler.AdmissionState().Used != 2 {
		t.Fatal("cancel released uncertain grant")
	}
}

// Legacy owner_restart rows represent trusted-KVM cleanup, not uncertain CI
// creates. A settler-capable engine must still reach Delete (and its VMI cleanup).
type legacyOwnerRestartStore struct {
	SchedulerStore
	cancellation model.RunCancellation
	completed    bool
}

func (s *legacyOwnerRestartStore) PendingRunCancellations(context.Context) ([]model.RunCancellation, error) {
	if s.completed {
		return nil, nil
	}
	return []model.RunCancellation{s.cancellation}, nil
}
func (s *legacyOwnerRestartStore) CompleteObservedRunCancellation(context.Context, model.RunCancellation) error {
	s.completed = true
	return nil
}
func TestWorkflowCreateLegacyOwnerRestartStillUsesDeleteWithSettlerEngine(t *testing.T) {
	f := newControlFixture(t)
	source, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "legacy-preflight", Repository: f.repo, Branch: "feature/preflight", SHA: strings.Repeat("f", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.ClaimNextRun(t.Context())
	if err != nil || run.ID != source.ID {
		t.Fatal(err)
	}
	run, err = f.store.SetRunJobName(t.Context(), run.ID, "preflight-workflow")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunInterrupted, Phase: "interrupted"}); err != nil {
		t.Fatal(err)
	}
	ledger := &legacyOwnerRestartStore{SchedulerStore: f.store, cancellation: model.RunCancellation{RunID: run.ID, JobName: run.JobName, Reason: "owner_restart", CreatedAt: time.Now()}}
	f.scheduler.store = ledger
	jobs := &pendingCreateJobs{fakeJobs: f.jobs, settle: func(context.Context, string, string, time.Time) (bool, error) {
		t.Fatal("trusted-KVM owner_restart bypassed VMI Delete cleanup")
		return false, nil
	}}
	f.scheduler.jobs = jobs
	if err := f.scheduler.completePendingCancellations(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !ledger.completed || len(f.jobs.deleted) != 1 || f.jobs.deleted[0] != run.JobName {
		t.Fatalf("legacy cleanup not deleted: completed=%t deleted=%v", ledger.completed, f.jobs.deleted)
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

func (s *deferredPendingStore) MarkRunCreatePending(ctx context.Context, id, name string) error {
	if s.fail {
		return context.DeadlineExceeded
	}
	return s.SchedulerStore.MarkRunCreatePending(ctx, id, name)
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
	if f.scheduler.AdmissionState().Used != 2 {
		t.Fatal("persistence outage freed grant")
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
				if _, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "superseding", Repository: f.repo, Branch: "feature/pending", SHA: strings.Repeat("c", 40), Actor: "pusher"}); err != nil {
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
			if calls != 0 || f.scheduler.AdmissionState().Used != 2 {
				t.Fatal("push/cancel settled inline or released grant")
			}
			f.scheduler.settleOneCreate(t.Context())
			run, err := f.store.Run(t.Context(), source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !run.Preempted() || isPendingCreate(run) || f.scheduler.pendingCreateCount() != 0 || f.scheduler.AdmissionState().Used != 0 {
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
	if response.DisplayStatus != "infra-pending" || response.Run.Phase != "infrastructure-pending" || !strings.Contains(response.DisplayReason, "cleanup pending") || response.Scheduling.State != "infra-pending" {
		t.Fatalf("pending status hidden: %+v", response)
	}
}
