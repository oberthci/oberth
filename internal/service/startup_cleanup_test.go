package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type retryingStartupJobs struct {
	recoveryJobs
	calls   atomic.Int32
	failed  chan struct{}
	proceed chan struct{}
}

func (j *retryingStartupJobs) Delete(ctx context.Context, name, id string) error {
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 2*time.Minute {
		return errors.New("startup cleanup has no bounded attempt")
	}
	if j.calls.Add(1) == 1 {
		close(j.failed)
		return errors.New("cleanup API temporarily unavailable")
	}
	select {
	case <-j.proceed:
		return nil
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
