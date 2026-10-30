package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type deadlineBusyError int

func (e deadlineBusyError) Code() int     { return int(e) }
func (e deadlineBusyError) Error() string { return "injected SQLite contention" }

type deadlineJobs struct {
	recoveryJobs
	creates atomic.Int64
	waits   atomic.Int64
	passed  bool
}

func (j *deadlineJobs) CreateCI(context.Context, JobRequest) error { j.creates.Add(1); return nil }
func (j *deadlineJobs) PipelineConcurrencyGroup(context.Context, JobRequest) (string, error) {
	return "db-deadline", nil
}
func (j *deadlineJobs) PipelineCredentialed(context.Context, JobRequest) (bool, error) {
	return true, nil
}
func (j *deadlineJobs) Wait(context.Context, string, io.Writer) (JobResult, error) {
	j.waits.Add(1)
	if j.passed {
		return JobResult{Status: model.RunPassed, Phase: "job"}, nil
	}
	return JobResult{Status: model.RunFailed, Phase: "job", Error: "test failed", Steps: []model.StepResult{{Burn: "build", Step: "test", Status: model.StepFailed}}}, nil
}

func deadlineScope(ctx context.Context, scheduler *Scheduler) context.Context {
	return context.WithValue(ctx, databaseRetryKey{}, &databaseRetryScope{lifetime: ctx, owner: scheduler})
}
func faultScheduler(t *testing.T, fixture *recoveryFixture, jobs JobController, operation string, fault error) (*Scheduler, *deadlineFaultStore) {
	t.Helper()
	scheduler := fixture.scheduler(t, jobs)
	database := &deadlineFaultStore{Store: fixture.store, failures: map[string]int{operation: 2}, calls: make(map[string]int), faultError: fault}
	scheduler.store = database
	scheduler.workspaces.owners = scheduler.database()
	return scheduler, database
}
func startDeadlineScheduler(t *testing.T, scheduler *Scheduler) (<-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("scheduler exited: %v", err)
			}
		case <-time.After(testStartTimeout(15 * time.Second)):
			t.Error("scheduler failed to stop")
		}
	})
	return done, cancel
}
func awaitDeadlineRun(t *testing.T, fixture *recoveryFixture, id string, done <-chan error) model.Run {
	t.Helper()
	timer := time.NewTimer(testStartTimeout(15 * time.Second))
	defer timer.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("DB failure reached fatal completion: %v", err)
		case <-timer.C:
			t.Fatal("run did not converge after DB recovery")
		case <-tick.C:
			run, err := fixture.store.Run(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status.Terminal() {
				return run
			}
		}
	}
}

// Exercise actual Run startup, dispatch, submitted worker and finalization;
// checking only adapter returns would miss a fatal completion-channel escape.
func TestDatabaseDeadlineSchedulerConverges(t *testing.T) {
	operations := []string{"RunningRunsWithJobs", "PendingRunCancellations", "PendingPublications", "RunWorkspaceCleanupCandidates", "PromotionWorkspaceCleanupCandidates", "ClaimNextRun", "Run", "Repository", "Upstream", "SetRunJobName", "SetRunConcurrencyGroup", "SetRunCredentialed", "MarkRunAdmitted", "MarkRunSubmitted", "PutStepResult", "FinishRun", "CompleteSupersededRunCancellationWithoutJob", "RunWorkspaceCleanupEligible"}
	for _, operation := range operations {
		for _, fault := range []struct {
			name string
			err  error
		}{{"deadline", context.DeadlineExceeded}, {"busy", deadlineBusyError(5)}} {
			t.Run(operation+"/"+fault.name, func(t *testing.T) {
				t.Parallel()
				fixture := newRecoveryFixture(t)
				queued, err := fixture.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "db-deadline", SHA: strings.Repeat("a", 40), Actor: "test", Trigger: "branch"})
				if err != nil {
					t.Fatal(err)
				}
				jobs := &deadlineJobs{}
				scheduler, database := faultScheduler(t, fixture, jobs, operation, fault.err)
				done, _ := startDeadlineScheduler(t, scheduler)
				run := awaitDeadlineRun(t, fixture, queued.ID, done)
				if run.Status != model.RunFailed || run.Phase != "job" || run.Error != "test failed" {
					t.Fatalf("DB stall changed execution result: %+v", run)
				}
				// Terminal commit can precede cleanup; wait for both injected failures to be
				// retried before asserting convergence of the worker's completion path.
				deadline := time.NewTimer(testStartTimeout(10 * time.Second))
				defer deadline.Stop()
				for database.count(operation) < 3 {
					select {
					case <-deadline.C:
						t.Fatalf("guard not reached/retried: %s calls=%d", operation, database.count(operation))
					case err := <-done:
						t.Fatalf("fatal completion: %v", err)
					case <-time.After(5 * time.Millisecond):
					}
				}
				if scheduler.DatabaseRetries() < 2 || jobs.creates.Load() != 1 || jobs.waits.Load() != 1 {
					t.Fatalf("operation replayed or failures uncounted: retries=%d creates=%d waits=%d", scheduler.DatabaseRetries(), jobs.creates.Load(), jobs.waits.Load())
				}
			})
		}
	}
}

func TestDatabaseDeadlinePublicationDoesNotReplayJobOrPush(t *testing.T) {
	testDatabasePublicationDoesNotReplayJobOrPush(t, context.DeadlineExceeded)
}
func TestDatabaseDeadlineBusyPublicationDoesNotReplayJobOrPush(t *testing.T) {
	testDatabasePublicationDoesNotReplayJobOrPush(t, deadlineBusyError(5))
}
func testDatabasePublicationDoesNotReplayJobOrPush(t *testing.T, failure error) {
	for _, operation := range []string{"BeginPublication", "Publication", "SetPublicationPredecessor", "FinalizePublication"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			fixture := newRecoveryFixture(t)
			queued, err := fixture.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("a", 40), Actor: "test", Trigger: "branch"})
			if err != nil {
				t.Fatal(err)
			}
			jobs := &deadlineJobs{passed: true}
			scheduler, database := faultScheduler(t, fixture, jobs, operation, failure)
			git := &deadlineGit{}
			scheduler.git = git
			done, _ := startDeadlineScheduler(t, scheduler)
			run := awaitDeadlineRun(t, fixture, queued.ID, done)
			if run.Status != model.RunPassed || database.count(operation) < 3 || jobs.creates.Load() != 1 || jobs.waits.Load() != 1 || git.pushes.Load() != 1 {
				t.Fatalf("publication failed/replayed: run=%+v db_calls=%d creates=%d waits=%d pushes=%d", run, database.count(operation), jobs.creates.Load(), jobs.waits.Load(), git.pushes.Load())
			}
		})
	}
}

type deadlineGit struct {
	controlGit
	pushes atomic.Int64
}

func (g *deadlineGit) PushPromotion(context.Context, string, string, string) error {
	g.pushes.Add(1)
	return nil
}

func TestDatabaseDeadlineCancellationContinuesLiveDispatch(t *testing.T) {
	testDatabaseCancellationContinuesLiveDispatch(t, context.DeadlineExceeded)
}
func TestDatabaseDeadlineBusyCancellationContinuesLiveDispatch(t *testing.T) {
	testDatabaseCancellationContinuesLiveDispatch(t, deadlineBusyError(5))
}
func testDatabaseCancellationContinuesLiveDispatch(t *testing.T, failure error) {
	for _, operation := range []string{"PendingRunCancellations", "Run", "CompleteObservedRunCancellation"} {
		t.Run(operation, func(t *testing.T) {
			fixture := newGroupFixture(t, 2)
			database := &deadlineFaultStore{Store: fixture.store, failures: make(map[string]int), calls: make(map[string]int), faultError: failure}
			fixture.scheduler.store = database
			fixture.scheduler.workspaces.owners = fixture.scheduler.database()
			done, _ := startDeadlineScheduler(t, fixture.scheduler)
			old := fixture.branch(t, fixture.beacon, "supersede-db", "a")
			awaitDeadlineCreated(t, fixture.engine, done)
			database.mu.Lock()
			before := database.calls[operation]
			database.failures[operation] = 2
			database.mu.Unlock()
			successor, err := fixture.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: fixture.beacon.ID, RefKind: model.RefBranch, Ref: "supersede-db", SHA: strings.Repeat("b", 40), Actor: "test", Trigger: "branch"})
			if err != nil {
				t.Fatal(err)
			}
			fixture.scheduler.NotifyQueue()
			created := awaitDeadlineCreated(t, fixture.engine, done)
			if created != successor.ID {
				t.Fatalf("successor not dispatched: %v", created)
			}
			timer := time.NewTimer(testStartTimeout(15 * time.Second))
			defer timer.Stop()
		pendingLoop:
			for {
				select {
				case err := <-done:
					t.Fatalf("cancellation DB error reached fatal completion: %v", err)
				case <-timer.C:
					t.Fatal("cancellation failed to converge")
				case <-time.After(5 * time.Millisecond):
					pending, err := fixture.store.PendingRunCancellations(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if len(pending) == 0 {
						break pendingLoop
					}
				}
			}
			fixture.engine.finish(t, successor.ID)
			if run := awaitDeadlineRun(t, &recoveryFixture{store: fixture.store}, successor.ID, done); run.Status != model.RunFailed || run.Phase != "failed" || run.FailedStep != "unit" {
				t.Fatal("successor failed", run)
			}
			if run := fixture.run(t, old.ID); run.Status != model.RunInterrupted || run.SupersededBy != successor.ID {
				t.Fatal("superseded run lost", run)
			}
			if database.count(operation) < before+3 || fixture.scheduler.DatabaseRetries() < 2 {
				t.Fatal("cancellation guard not exercised", database.count(operation), fixture.scheduler.DatabaseRetries())
			}
		})
	}
}

func TestDatabaseDeadlineClassificationAndRequestBoundary(t *testing.T) {
	invariant := errors.New("immutable admission binding mismatch")
	for _, test := range []struct {
		err   error
		retry bool
	}{{context.DeadlineExceeded, true}, {fmt.Errorf("load run: %w", context.DeadlineExceeded), true}, {deadlineBusyError(5), true}, {deadlineBusyError(6 | 256), true}, {errors.Join(context.DeadlineExceeded, deadlineBusyError(5)), true}, {errors.Join(context.DeadlineExceeded, invariant), false}, {store.ErrInvalidState, false}, {store.ErrInvalid, false}, {invariant, false}, {context.Canceled, false}} {
		if got := transientDatabaseError(test.err); got != test.retry {
			t.Errorf("classify %v = %v", test.err, got)
		}
	}
	for _, err := range []error{sql.ErrTxDone, context.Canceled, deadlineBusyError(9)} {
		if transientDatabaseAttemptError(err, false) || !transientDatabaseAttemptError(err, true) || transientDatabaseAttemptError(errors.Join(err, invariant), true) {
			t.Errorf("own-deadline classification unsafe: %v", err)
		}
	}

	if !recoverablePreflightError(errors.Join(context.Canceled, context.DeadlineExceeded)) || recoverablePreflightError(context.Canceled) || recoverablePreflightError(errors.Join(context.Canceled, context.DeadlineExceeded, invariant)) {
		t.Fatal("trusted cancellation masked an invariant or lost DB failure")
	}
	scheduler := &Scheduler{}
	calls := 0
	if err := retryDatabaseError(t.Context(), "request", func(context.Context) error { calls++; return context.DeadlineExceeded }); !errors.Is(err, context.DeadlineExceeded) || calls != 1 || scheduler.DatabaseRetries() != 0 {
		t.Fatal("client request silently retried", err, calls)
	}
	for _, err := range []error{invariant, errors.Join(context.DeadlineExceeded, invariant)} {
		calls = 0
		checkCtx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		got := retryDatabaseError(deadlineScope(checkCtx, scheduler), "invariant", func(context.Context) error { calls++; return err })
		if !errors.Is(got, err) || calls != 1 {
			t.Fatal("invariant was hidden", got, calls)
		}
	}
}

func TestDatabaseDeadlineRecoveryAndShutdown(t *testing.T) {
	scheduler := &Scheduler{}
	calls := 0
	if err := scheduler.RetryStorageRecovery(t.Context(), "activation", func(ctx context.Context) error {
		calls++
		if calls < 3 {
			return context.DeadlineExceeded
		}
		return nil
	}); err != nil || calls != 3 || scheduler.DatabaseRetries() != 2 {
		t.Fatal("activation failed to converge", err, calls)
	}
	ctx, cancel := context.WithCancel(t.Context())
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- retryDatabaseError(deadlineScope(ctx, scheduler), "persistent-busy", func(context.Context) error {
			select {
			case <-entered:
			default:
				close(entered)
			}
			return deadlineBusyError(5)
		})
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(testStartTimeout(time.Second)):
		t.Fatal("backoff ignored shutdown")
	}
}

func (s *deadlineFaultStore) RunsByIDs(ctx context.Context, ids []string) (map[string]model.Run, error) {
	if err := s.fault("RunsByIDs"); err != nil {
		return nil, err
	}
	return s.Store.RunsByIDs(ctx, ids)
}
func (s *deadlineFaultStore) NextQueuedRun(ctx context.Context) (model.Run, error) {
	if err := s.fault("NextQueuedRun"); err != nil {
		return model.Run{}, err
	}
	return s.Store.NextQueuedRun(ctx)
}
func TestDatabaseDeadlinePriorityReadsConverge(t *testing.T) {
	testDatabasePriorityReadsConverge(t, context.DeadlineExceeded)
}
func TestDatabaseDeadlineBusyPriorityReadsConverge(t *testing.T) {
	testDatabasePriorityReadsConverge(t, deadlineBusyError(5))
}
func testDatabasePriorityReadsConverge(t *testing.T, failure error) {
	for _, operation := range []string{"RunsByIDs", "NextQueuedRun"} {
		t.Run(operation, func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			scheduler, fault := faultScheduler(t, fixture, &recoveryJobs{}, operation, failure)
			// One prepared waiter makes both durable priority reads relevant. It owns
			// no Workflow and no grant; cancellation removes it at the end of the test.
			reservation := scheduler.admission.reserveRun(model.Run{ID: "priority-waiter", QueueSequence: 1, RepoID: fixture.repo.ID})
			scheduler.admission.mu.Lock()
			reservation.prepared = true
			scheduler.admission.waiters = append(scheduler.admission.waiters, reservation)
			scheduler.admission.mu.Unlock()
			defer reservation.cancel()
			ctx := deadlineScope(t.Context(), scheduler)
			var err error
			if operation == "RunsByIDs" {
				err = scheduler.refreshPriorities(ctx)
			} else {
				_, err = scheduler.priorityDispatchAvailable(ctx, 0)
			}
			if err != nil || fault.count(operation) != 3 || scheduler.DatabaseRetries() != 2 {
				t.Fatal("priority guard failed", err, fault.count(operation), scheduler.DatabaseRetries())
			}
		})
	}
}

func TestDatabaseDeadlineWaitingDoesNotTripLiveness(t *testing.T) {
	scheduler := &Scheduler{}
	now := time.Now()
	scheduler.dispatchProgress.Beat()
	ctx, cancel := context.WithTimeout(t.Context(), testStartTimeout(time.Second))
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	var calls atomic.Int64
	go func() {
		done <- retryDatabaseError(withDispatchProgress(deadlineScope(ctx, scheduler), &scheduler.dispatchProgress), "failed-read", func(context.Context) error {
			if calls.Add(1) == 1 {
				close(entered)
			}
			return context.DeadlineExceeded
		})
	}()
	<-entered
	// Observe the explicit backoff suspension, not a timing assumption about
	// when the retry goroutine runs after sending its first failure signal.
	for scheduler.liveAt(now.Add(24*time.Hour)) != nil {
		select {
		case <-ctx.Done():
			t.Fatal("intentional retry backoff never suspended liveness")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(testStartTimeout(time.Second)):
		t.Fatal("DB backoff did not cancel")
	}
}

type deadlineJoinStore struct {
	*store.Store
	claims  atomic.Int64
	blocked chan struct{}
	once    sync.Once
}

func (s *deadlineJoinStore) ClaimNextRun(ctx context.Context) (model.Run, error) {
	if s.claims.Add(1) == 1 {
		return s.Store.ClaimNextRun(ctx)
	}
	select {
	case <-s.blocked:
		return model.Run{}, store.ErrInvalidState
	case <-ctx.Done():
		return model.Run{}, ctx.Err()
	}
}
func (s *deadlineJoinStore) Run(context.Context, string) (model.Run, error) {
	s.once.Do(func() { close(s.blocked) })
	return model.Run{}, context.DeadlineExceeded
}
func TestDatabaseDeadlineFatalInvariantCancelsOtherRetryingWorkers(t *testing.T) {
	fixture := newRecoveryFixture(t)
	if _, err := fixture.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "pending", SHA: strings.Repeat("a", 40), Actor: "test", Trigger: "branch"}); err != nil {
		t.Fatal(err)
	}
	scheduler := fixture.scheduler(t, &deadlineJobs{})
	scheduler.maxConcurrent = 2
	scheduler.store = &deadlineJoinStore{Store: fixture.store, blocked: make(chan struct{})}
	// This deadline is a watchdog, not the expected completion. A fatal invariant
	// must cancel the other worker's retry scope before joining it.
	ctx, cancel := context.WithTimeout(t.Context(), testStartTimeout(2*time.Second))
	defer cancel()
	if err := scheduler.Run(ctx); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("invariant blocked behind DB retry until shutdown: %v", err)
	}
}

func awaitDeadlineCreated(t *testing.T, engine *groupEngine, done <-chan error) string {
	t.Helper()
	select {
	case id := <-engine.created:
		return id
	case err := <-done:
		t.Fatalf("DB failure stopped live dispatch: %v", err)
	case <-time.After(testStartTimeout(15 * time.Second)):
		t.Fatal("live dispatch did not converge")
	}
	return ""
}
