package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type ownedWorkflowMismatch struct{}

func (*ownedWorkflowMismatch) Error() string              { return "Workflow spec mismatch" }
func (*ownedWorkflowMismatch) AmbiguousCreate() bool      { return true }
func (*ownedWorkflowMismatch) WorkflowSpecMismatch() bool { return true }

func TestWorkflowSpecMismatchStaysRunLocalUntilCleanup(t *testing.T) {
	f := newControlFixture(t, JobResult{Status: model.RunFailed, Phase: "infrastructure"})
	// Keep this one-attempt cleanup contract separate from the default-on retry below.

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
	run, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: "mismatch", Repository: f.repo, Branch: "feature/mismatch", SHA: strings.Repeat("a", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	awaitCreateState(t, f.scheduler, run.ID, 1, false)
	current, err := f.store.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunRunning || !strings.Contains(current.Error, "Workflow spec mismatch") {
		t.Fatalf("mismatch diagnostic or pending ownership lost: %+v %v", current, err)
	}
	other, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: "after-mismatch", Repository: f.repo, Branch: "feature/other", SHA: strings.Repeat("b", 40), Actor: "pusher"})
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
	if f.scheduler.submissionStateForTest().Runs != 0 {
		t.Fatal("confirmed cleanup retained capacity")
	}
	if pending, err := f.store.PendingRunCancellations(t.Context()); err != nil || len(pending) != 0 {
		t.Fatal(errors.New("confirmed cleanup left a cancellation"), err)
	}
}

func TestWorkflowMismatchCleanupReleasesOriginalBeforeManualRetry(t *testing.T) {
	f := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "passed"})
	resume := make(chan struct{})
	releaseRetry := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(releaseRetry)
	entered := make(chan WorkflowRequest, 1)
	var retries atomic.Int32
	jobs := &pendingCreateJobs{fakeJobs: f.jobs}
	var firstCreate atomic.Bool
	firstCreate.Store(true)
	jobs.createHook = func(ctx context.Context, request WorkflowRequest) error {
		if firstCreate.Swap(false) {
			return &ownedWorkflowMismatch{}
		}
		retries.Add(1)
		select {
		case entered <- request:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-resume:
			return f.jobs.CreateWorkflow(ctx, request)
		case <-ctx.Done():
			return ctx.Err()
		}
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
	done, stop := startCreateScheduler(t, f.scheduler)
	source, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: "mismatch-retry", Repository: f.repo, Branch: "feature/mismatch-retry", SHA: strings.Repeat("a", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	awaitCreateState(t, f.scheduler, source.ID, 1, false)
	original, err := f.store.Run(t.Context(), source.ID)
	if err != nil || original.Status != model.RunRunning || retries.Load() != 0 {
		t.Fatalf("uncertain cleanup authorized a retry: %+v retries=%d err=%v", original, retries.Load(), err)
	}
	state := f.scheduler.submissionStateForTest()
	if state.Runs != 1 || len(state.Reservations) != 1 || state.Reservations[0].RunID != source.ID {
		t.Fatalf("original cleanup lost its sole reservation: %+v", state)
	}
	gone.Store(true)
	clock.Add(int64(2 * time.Minute))
	select {
	case ticks <- f.scheduler.createTime():
	case <-time.After(testStartTimeout(time.Second)):
		t.Fatal("cleanup worker stopped")
	}
	awaitCreateState(t, f.scheduler, source.ID, 0, true)
	control := f.api(t)
	control.runReruns = f.store
	control.refs = stubRefResolver{branches: map[string]map[string]string{control.cacheInputFor(t.Context(), f.repo): {source.Ref: source.SHA}}}
	manual, err := control.rerunRun(t.Context(), api.Actor{Identity: "pusher"}, source.ID, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	var retry WorkflowRequest
	select {
	case retry = <-entered:
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("confirmed cleanup did not admit the manual retry")
	}
	original, err = f.store.Run(t.Context(), source.ID)
	if err != nil || original.Status != model.RunFailed || retry.Run.ID != manual.RunID || retry.Run.ID == source.ID || !strings.Contains(original.Error, "cleanup confirmed") {
		t.Fatalf("retry lost original cleanup verdict or lineage: original=%+v retry=%+v err=%v", original, retry.Run, err)
	}
	state = f.scheduler.submissionStateForTest()
	if state.Runs != 1 || len(state.Reservations) != 1 || state.Reservations[0].RunID != retry.Run.ID {
		t.Fatalf("original reservation survived instead of only the retry: %+v", state)
	}
	releaseRetry()
	waitForFixtureRunStatus(t, f, retry.Run.ID, model.RunPassed)
	stop()
	select {
	case err := <-done:
		done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("scheduler did not join after retry completion")
	}
	state = f.scheduler.submissionStateForTest()
	if state.Runs != 0 || len(state.Reservations) != 0 || f.scheduler.pendingCreateCount() != 0 || retries.Load() != 1 {
		t.Fatalf("completed retry retained capacity or repeated: state=%+v retries=%d", state, retries.Load())
	}
	if pending, err := f.store.PendingRunCancellations(t.Context()); err != nil || len(pending) != 0 {
		t.Fatalf("completed retry left cancellations: %+v err=%v", pending, err)
	}
}
