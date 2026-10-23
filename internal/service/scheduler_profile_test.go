package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type profileGatedJobs struct {
	*concurrentJobs
	blocked   atomic.Bool
	waiting   chan struct{}
	recovered chan struct{}
}

func (jobs *profileGatedJobs) AwaitAdmission(ctx context.Context) error {
	if !jobs.blocked.Load() {
		return ctx.Err()
	}
	select {
	case jobs.waiting <- struct{}{}:
	default:
	}
	select {
	case <-jobs.recovered:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSchedulerPausesAdmissionWithoutStoppingExistingWork(t *testing.T) {
	fixture := newConcurrencyFixture(t, periapsis.M, 2)
	jobs := &profileGatedJobs{concurrentJobs: fixture.jobs, waiting: make(chan struct{}, 2), recovered: make(chan struct{})}
	fixture.scheduler.jobs = jobs
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	fixture.enqueue(t, 0)
	go func() { finished <- fixture.scheduler.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(time.Minute):
			t.Error("scheduler did not stop")
		}
	}()
	var initialRun string
	select {
	case initialRun = <-jobs.admitted:
	case <-finished:
		t.Fatal("scheduler exited")
	case <-time.After(time.Minute):
		t.Fatal("initial job not admitted")
	}
	jobs.blocked.Store(true)
	fixture.enqueue(t, 1)
	select {
	case <-jobs.waiting:
	case err := <-finished:
		t.Fatalf("profile outage terminated scheduler: %v", err)
	case <-time.After(time.Minute):
		t.Fatal("admission gate not called")
	}
	select {
	case <-jobs.admitted:
		t.Fatal("new job admitted while profile unverified")
	default:
	}
	// An active worker can finish while new admission waits for verification.
	close(jobs.release)
	awaitRunStatus(t, fixture, initialRun, model.RunPassed)
	select {
	case err := <-finished:
		t.Fatalf("profile outage stopped scheduler: %v", err)
	default:
	}
	jobs.blocked.Store(false)
	close(jobs.recovered)
	select {
	case <-jobs.admitted:
	case err := <-finished:
		t.Fatalf("scheduler failed to recover: %v", err)
	case <-time.After(time.Minute):
		t.Fatal("admission did not resume")
	}
}
