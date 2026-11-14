package service

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// ProcessNext feeds one persisted fixture into the synchronous execution path.
// Live producers use AcceptEnqueue; production has no row dispatcher.
func (scheduler *Scheduler) ProcessNext(ctx context.Context) error {
	if err := scheduler.requireMutation(ctx); err != nil {
		return err
	}
	history, ok := fixtureHistory(scheduler)
	if !ok {
		return store.ErrNotFound
	}
	runs, err := history.ListRecentRuns(ctx, model.RunListFilter{Status: model.RunStatusFilter{"queued", "running"}, Limit: 500})
	if err != nil {
		return err
	}
	var chosen model.Run
	for _, r := range runs {
		if (r.Status == model.RunQueued || (r.Status == model.RunRunning && r.WorkflowName == "" && r.Phase == "waiting")) && (chosen.ID == "" || r.QueueSequence < chosen.QueueSequence) {
			chosen = r
		}
	}
	if chosen.ID == "" {
		return store.ErrNotFound
	}
	run, err := scheduler.database().StartRun(ctx, chosen.ID)
	if err != nil {
		return err
	}
	return scheduler.executeAndCleanup(ctx, run)
}

func (scheduler *Scheduler) recordCIForTest(ctx context.Context, request CIRequest) (model.EnqueueRunResult, error) {
	result, err := scheduler.recordCI(ctx, request, nil)
	if err == nil {
		if running, ok := acceptedFixtureServers.Load(scheduler); ok {
			running.(func(model.EnqueueRunResult))(result.Enqueued)
		}
	}
	return result.Enqueued, err
}
func (scheduler *Scheduler) recordReleaseForTest(ctx context.Context, request ReleaseRequest) (model.EnqueueRunResult, error) {
	result, err := scheduler.recordRelease(ctx, request)
	if err == nil {
		if running, ok := acceptedFixtureServers.Load(scheduler); ok {
			running.(func(model.EnqueueRunResult))(result)
		}
	}
	return result, err
}

func (scheduler *Scheduler) executeAndCleanup(ctx context.Context, run model.Run) error {
	return scheduler.executeAndCleanupWithGate(ctx, run, scheduler.requireMutation)
}

func (scheduler *Scheduler) executeAndCleanupWithGate(ctx context.Context, run model.Run, require mutationRequirement) error {
	reservation := scheduler.submissions.trackRun(run)
	return scheduler.executeAndCleanupTrackedWithGate(ctx, run, require, reservation)
}

func (scheduler *Scheduler) executeAndCleanupTrackedWithGate(
	ctx context.Context,
	run model.Run,
	require mutationRequirement,
	reservation *submissionOwner,
) error {
	defer reservation.cancel()
	finishEvidence := scheduler.beginEvidenceExecution(run.ID)
	defer finishEvidence()
	// A cancellation can terminalize a claim before its goroutine starts. Do
	// not create logs after a purge that completed in that dispatch window.
	current, executeErr := scheduler.database().Run(ctx, run.ID)
	if executeErr == nil && current.Status.Active() {
		executeErr = scheduler.execute(ctx, run, require, reservation)
	}
	return scheduler.cleanupExecution(ctx, run, executeErr)
}

func (scheduler *Scheduler) execute(ctx context.Context, run model.Run, require mutationRequirement, reservation *submissionOwner) error {
	return scheduler.submitWorkflow(ctx, run, require, reservation, func(run model.Run, repository model.Repository, observation runObservation) error {
		return scheduler.observeRun(ctx, run, repository, require, reservation, observation)
	})
}

var acceptedFixtureServers sync.Map

type fixtureRunReader interface {
	ListRecentRuns(context.Context, model.RunListFilter) ([]model.Run, error)
}

var fixtureHistories sync.Map

func fixtureHistory(s *Scheduler) (fixtureRunReader, bool) {
	if v, ok := fixtureHistories.Load(s); ok {
		return v.(fixtureRunReader), true
	}
	h, ok := s.store.(fixtureRunReader)
	return h, ok
}
func (scheduler *Scheduler) runAcceptedFixtures(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	for !scheduler.Recovered() {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return <-done
		case <-time.After(time.Millisecond):
		}
	}
	var requests sync.WaitGroup
	var requestMu sync.Mutex
	accepting := true
	seen := map[string]bool{}
	errors := make(chan error, 1)
	submit := func(r model.EnqueueRunResult) {
		requestMu.Lock()
		defer requestMu.Unlock()
		if !accepting || seen[r.ID] {
			return
		}
		seen[r.ID] = true
		requests.Add(1)
		go func() {
			defer requests.Done()
			if err := scheduler.AcceptEnqueue(deadlineScope(ctx, scheduler), r); err != nil {
				select {
				case errors <- err:
				default:
				}
			}
		}()
	}
	acceptedFixtureServers.Store(scheduler, submit)
	defer func() {
		acceptedFixtureServers.Delete(scheduler)
		requestMu.Lock()
		accepting = false
		requestMu.Unlock()
		cancel()
		requests.Wait()
	}()
	if history, ok := fixtureHistory(scheduler); ok {
		runs, err := history.ListRecentRuns(ctx, model.RunListFilter{Status: model.RunStatusFilter{"queued", "running"}, Limit: 500})
		if err != nil {
			cancel()
			stopped := <-done
			if parent.Err() != nil {
				return stopped
			}
			return err
		}
		for i := len(runs) - 1; i >= 0; i-- {
			if runs[i].Status != model.RunQueued && (runs[i].Status != model.RunRunning || runs[i].WorkflowName != "" || (runs[i].Phase != "waiting" && !runs[i].Cancelled())) {
				continue
			}
			submit(model.EnqueueRunResult{Run: runs[i]})
		}
	}
	select {
	case err := <-done:
		return err
	case err := <-errors:
		cancel()
		<-done
		return err
	}
}

// fixtureEnqueues lets downstream tests prepare history independently of the
// live accepting handler. Running fixture servers submit immediately.
type fixtureEnqueues struct{ *Scheduler }

func (e fixtureEnqueues) AcceptEnqueue(ctx context.Context, r model.EnqueueRunResult) error {
	if submit, ok := acceptedFixtureServers.Load(e.Scheduler); ok {
		submit.(func(model.EnqueueRunResult))(r)
	}
	return nil
}

func awaitSchedulerRecovered(t *testing.T, scheduler *Scheduler) {
	t.Helper()
	deadline := time.NewTimer(testStartTimeout(5 * time.Second))
	defer deadline.Stop()
	for !scheduler.Recovered() {
		select {
		case <-deadline.C:
			t.Fatal("observer did not recover")
		case <-time.After(time.Millisecond):
		}
	}
}

func queueEmpty(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrNotFound)
}

func (e fixtureEnqueues) EnqueueCI(ctx context.Context, r CIRequest) (model.EnqueueRunResult, error) {
	return e.recordCIForTest(ctx, r)
}
