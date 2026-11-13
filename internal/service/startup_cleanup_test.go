package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/storetest"
)

type retryingStartupJobs struct {
	recoveryJobs
	calls   atomic.Int32
	failed  chan struct{}
	proceed chan struct{}
}

func (j *retryingStartupJobs) Stop(ctx context.Context, name, id string) error {
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 2*time.Minute {
		return errors.New("startup cleanup has no bounded attempt")
	}
	if j.calls.Add(1) == 1 {
		close(j.failed)
		return errors.New("cleanup API temporarily unavailable")
	}
	select {
	case <-j.proceed:
		return j.recoveryJobs.Stop(ctx, name, id)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestStartupCancellationWaitsForObservedCleanup(t *testing.T) {
	f := newRecoveryFixture(t)
	run := f.enqueueAndClaim(t, "feature/cancelled", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	f.setWorkflowName(t, run.ID, "oberth-cancelled")
	if _, err := f.store.CancelRun(t.Context(), model.RunCancelRequest{RunID: run.ID, Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	jobs := &retryingStartupJobs{failed: make(chan struct{}), proceed: make(chan struct{})}
	scheduler := f.scheduler(t, jobs)
	recovered := make(chan struct{})
	scheduler.onStartupRecovered = func() { close(recovered) }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-jobs.failed:
	case err := <-done:
		done <- err
		t.Fatalf("startup exited instead of retrying cleanup: %v", err)
	case <-time.After(testTimeout(5 * time.Second)):
		t.Fatal("cleanup was not attempted")
	}
	pending, err := f.store.PendingRunCancellations(t.Context())
	if err != nil || len(pending) != 1 || pending[0].RunID != run.ID {
		t.Fatalf("uncertain cleanup lost its obligation: %+v %v", pending, err)
	}
	if scheduler.Recovered() {
		t.Fatal("scheduler ready before cleanup")
	}
	select {
	case err := <-done:
		done <- err
		t.Fatalf("temporary cleanup failure stopped scheduler: %v", err)
	default:
	}
	close(jobs.proceed)
	select {
	case <-recovered:
	case err := <-done:
		done <- err
		t.Fatalf("startup did not recover: %v", err)
	case <-time.After(testTimeout(5 * time.Second)):
		t.Fatal("cleanup did not retry")
	}
	awaitCreateState(t, scheduler, run.ID, 0, true)
	pending, err = f.store.PendingRunCancellations(t.Context())
	if err != nil || len(pending) != 0 || jobs.calls.Load() < 2 {
		t.Fatalf("cleanup did not finish: %+v %v calls=%d", pending, err, jobs.calls.Load())
	}
}

func TestStartupCleanupAttemptStopsOnShutdown(t *testing.T) {
	scheduler := &Scheduler{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- scheduler.waitForStartupCleanup(ctx, "cancellations", func(attempt context.Context) error {
			if deadline, ok := attempt.Deadline(); !ok || time.Until(deadline) > startupCleanupAttemptBudget {
				return errors.New("unbounded cleanup attempt")
			}
			close(entered)
			<-attempt.Done()
			return attempt.Err()
		})
	}()
	select {
	case <-entered:
	case <-time.After(testTimeout(5 * time.Second)):
		t.Fatal("cleanup not attempted")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(testTimeout(5 * time.Second)):
		t.Fatal("shutdown did not interrupt cleanup")
	}
}

type steadyCancellationJobs struct {
	recoveryJobs
	calls   atomic.Int32
	failed  chan struct{}
	deleted chan struct{}
}

func (j *steadyCancellationJobs) Stop(ctx context.Context, name, id string) error {
	switch j.calls.Add(1) {
	case 1:
		close(j.failed)
		return errors.New("kube-apiserver temporarily unavailable")
	case 2:
		close(j.deleted)
	}
	return j.recoveryJobs.Stop(ctx, name, id)
}

// After startup, a Workflow deletion the API refuses keeps the server up and
// the obligation pending, and the pass runs again on its own backoff.
func TestSteadyCancellationDeleteFailureRetriesWithoutExit(t *testing.T) {
	f := newRecoveryFixture(t)
	jobs := &steadyCancellationJobs{failed: make(chan struct{}), deleted: make(chan struct{})}
	scheduler := f.scheduler(t, jobs)
	// Create the cancellation once startup recovery is over and before the
	// dispatch loop starts, so neither startup cleanup nor a dispatch claim
	// can take it.
	recovered := make(chan error, 1)
	scheduler.onStartupRecovered = func() {
		recovered <- func() error {
			enqueued, err := storetest.EnqueueRun(f.store, context.Background(), model.RunSpec{
				RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/cancelled",
				SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host", Trigger: "branch",
			})
			if err != nil {
				return err
			}
			if _, err := f.store.ClaimNextRun(context.Background()); err != nil {
				return err
			}
			if _, err := f.store.SetRunWorkflowName(context.Background(), enqueued.ID, "oberth-cancelled"); err != nil {
				return err
			}
			_, err = f.store.CancelRun(context.Background(), model.RunCancelRequest{RunID: enqueued.ID, Actor: "operator"})
			return err
		}()
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case err := <-recovered:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-done:
		done <- err
		t.Fatalf("scheduler exited during startup: %v", err)
	case <-time.After(testTimeout(5 * time.Second)):
		t.Fatal("startup did not finish")
	}
	select {
	case <-jobs.failed:
	case err := <-done:
		done <- err
		t.Fatalf("scheduler exited before the deletion: %v", err)
	case <-time.After(testTimeout(5 * time.Second)):
		t.Fatal("deletion was not attempted")
	}
	select {
	case <-jobs.deleted:
	case err := <-done:
		done <- err
		t.Fatalf("one failed deletion stopped the scheduler: %v", err)
	case <-time.After(testTimeout(5 * time.Second)):
		t.Fatal("the cancellation pass did not retry on its own")
	}
	deadline := time.Now().Add(testTimeout(5 * time.Second))
	for {
		pending, err := f.store.PendingRunCancellations(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("obligation not completed after retry: %+v", pending)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		done <- err
		t.Fatalf("scheduler exited: %v", err)
	default:
	}
}

func TestOnlyPendingJobDeletions(t *testing.T) {
	pending := fmt.Errorf("Stop Workflow x: %w", &pendingWorkflowStopError{errors.New("api down")})
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"one deletion", pending, true},
		{"joined deletions", errors.Join(pending, pending), true},
		{"store error", errors.New("database is locked"), false},
		{"deletion beside store error", errors.Join(pending, errors.New("constraint failed")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := onlyPendingWorkflowStops(tc.err); got != tc.want {
				t.Fatalf("onlyPendingWorkflowStops = %v, want %v", got, tc.want)
			}
		})
	}
}

// Every worker call site of the cancellation pass routes its error through
// deferRefusedStops: refused deletions become a dispatch-loop retry,
// anything else, or a cancelled worker, keeps the error.
func TestDeferRefusedDeletionsWakesDispatchRetry(t *testing.T) {
	pending := fmt.Errorf("Stop Workflow x: %w", &pendingWorkflowStopError{errors.New("api down")})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		err     error
		want    error
		wakeups int
	}{
		{"refused deletion", context.Background(), pending, nil, 1},
		{"store error", context.Background(), errors.New("database is locked"), errors.New("database is locked"), 0},
		{"mixed", context.Background(), errors.Join(pending, errors.New("constraint")), errors.New("x"), 0},
		{"worker stopping", cancelled, pending, pending, 0},
		{"nil", context.Background(), nil, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var notified []string
			signals := NewSignals()
			signals.Observe(func(_, id string) { notified = append(notified, id) })
			scheduler := &Scheduler{wake: make(chan struct{}, 1), signals: signals}
			got := scheduler.deferRefusedStops(tc.ctx, "run", tc.err)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("deferRefusedStops = %v, want error=%t", got, tc.want != nil)
			}
			if len(scheduler.wake) != tc.wakeups {
				t.Fatalf("dispatch wakeups = %d, want %d", len(scheduler.wake), tc.wakeups)
			}
			// A deferred run is already preempted; its waiters are told now.
			if len(notified) != tc.wakeups || (tc.wakeups == 1 && notified[0] != "run") {
				t.Fatalf("run notifications = %v, want %d", notified, tc.wakeups)
			}
		})
	}
}

func (j *retryingStartupJobs) SettleCreate(ctx context.Context, name, id string, _ time.Time) (bool, error) {
	if err := j.Stop(ctx, name, id); err != nil {
		return false, err
	}
	if err := j.recoveryJobs.Stop(ctx, name, id); err != nil {
		return false, err
	}
	return true, nil
}
func (j *steadyCancellationJobs) SettleCreate(ctx context.Context, name, id string, _ time.Time) (bool, error) {
	if err := j.Stop(ctx, name, id); err != nil {
		return false, err
	}
	return true, nil
}
