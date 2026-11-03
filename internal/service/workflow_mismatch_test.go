package service

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type ownedWorkflowMismatch struct{}

func (*ownedWorkflowMismatch) Error() string              { return "Workflow spec mismatch" }
func (*ownedWorkflowMismatch) AmbiguousCreate() bool      { return true }
func (*ownedWorkflowMismatch) WorkflowSpecMismatch() bool { return true }

func TestWorkflowSpecMismatchStaysRunLocalUntilCleanup(t *testing.T) {
	f := newControlFixture(t, JobResult{Status: model.RunFailed, Phase: "infrastructure"})
	jobs := &pendingCreateJobs{fakeJobs: f.jobs}
	jobs.createHook = func(ctx context.Context, request WorkflowRequest) error {
		if request.Run.Ref == "feature/mismatch" {
			return &ownedWorkflowMismatch{}
		}
		return f.jobs.CreateWorkflow(ctx, request)
	}
	var gone atomic.Bool
	jobs.settle = func(context.Context, string, string, time.Time) (bool, error) {
		return gone.Load(), nil
	}
	f.scheduler.jobs = jobs
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	f.scheduler.createNow = func() time.Time { return time.Unix(0, clock.Load()) }
	ticks := make(chan time.Time)
	f.scheduler.createTicks = ticks
	done, _ := startCreateScheduler(t, f.scheduler)
	run, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "mismatch", Repository: f.repo, Branch: "feature/mismatch", SHA: strings.Repeat("a", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	awaitCreateState(t, f.scheduler, run.ID, 1, false)
	current, err := f.store.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunRunning || !strings.Contains(current.Error, "Workflow spec mismatch") {
		t.Fatalf("mismatch diagnostic or pending ownership lost: %+v %v", current, err)
	}
	if f.scheduler.AdmissionState().NumericBudget != nil {
		t.Fatal("owned Workflow released capacity before cleanup")
	}
	other, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "after-mismatch", Repository: f.repo, Branch: "feature/other", SHA: strings.Repeat("b", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	waitForFixtureRunStatus(t, f, other.ID, model.RunFailed)
	select {
	case err := <-done:
		done <- err
		t.Fatalf("one Workflow stopped the scheduler: %v", err)
	default:
	}
	if !f.scheduler.Recovered() {
		t.Fatal("one Workflow withdrew server readiness")
	}
	gone.Store(true)
	clock.Add(int64(2 * time.Minute))
	select {
	case ticks <- f.scheduler.createTime():
	case <-time.After(testStartTimeout(time.Second)):
		t.Fatal("cleanup worker stopped")
	}
	waitForFixtureRunStatus(t, f, run.ID, model.RunFailed)
	awaitCreateState(t, f.scheduler, run.ID, 0, true)
	current, err = f.store.Run(t.Context(), run.ID)
	if err != nil || !strings.Contains(current.Error, "Workflow spec mismatch") || !strings.Contains(current.Error, "cleanup confirmed") {
		t.Fatalf("settlement lost the named failure: %+v %v", current, err)
	}
	if f.scheduler.AdmissionState().Runs != 0 {
		t.Fatal("confirmed cleanup retained capacity")
	}
	if pending, err := f.store.PendingRunCancellations(t.Context()); err != nil || len(pending) != 0 {
		t.Fatal(errors.New("confirmed cleanup left a cancellation"), err)
	}
}
