package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// A late accepted POST is observed through the shared hub and stopped. This
// observer owns no submission retry or execution capacity.
func (scheduler *Scheduler) watchFailedCreate(request WorkflowRequest) {
	watcher, ok := scheduler.jobs.(interface {
		WatchFailedCreate(context.Context, WorkflowRequest) error
	})
	if !ok {
		return
	}
	scheduler.observationMu.Lock()
	defer scheduler.observationMu.Unlock()
	if scheduler.observationsClosed {
		return
	}
	scheduler.observers.Add(1)
	go func() {
		defer scheduler.observers.Done()
		ctx, cancel := context.WithTimeout(scheduler.observationCtx, createObservationWindow)
		defer cancel()
		if err := watcher.WatchFailedCreate(ctx, request); err != nil && !errors.Is(err, context.DeadlineExceeded) && scheduler.observationCtx.Err() == nil {
			log.Printf("WARN: failed Workflow create observation run=%s: %v", request.Run.ID, err)
		}
	}()
}

// Persist a failed accepting request after its client has disconnected.
func (scheduler *Scheduler) failAccept(ctx context.Context, run model.Run, cause error) {
	finish, cancel := boundedWorkspaceCleanupContext(ctx)
	defer cancel()
	current, err := scheduler.database().Run(finish, run.ID)
	if err == nil && current.Status.Active() {
		if err := scheduler.finishInfrastructureFailure(finish, current, model.Repository{}, fmt.Errorf("Workflow submission failed; request another attempt explicitly: %w", cause)); err != nil {
			log.Printf("WARN: record accepting request failure run=%s: %v", run.ID, err)
		}
	}
}

// WorkflowRunRef identifies a cached Workflow without exposing its authored spec.
type WorkflowRunRef struct{ RunID, WorkflowName string }

// Rearm from live objects and only the recent failed-create observation window.
// Historical cancellations whose Workflows are gone do no startup work.
func (scheduler *Scheduler) rearmFailedCreates(ctx context.Context) error {
	if _, ok := scheduler.jobs.(interface {
		WatchFailedCreate(context.Context, WorkflowRequest) error
	}); !ok {
		return nil
	}
	if adopter, ok := scheduler.jobs.(JobAdopter); ok {
		if err := adopter.WaitForWatchSync(ctx); err != nil {
			return err
		}
	}
	since := time.Now().Add(-createObservationWindow)
	recent, err := scheduler.database().FailedWorkflowCreates(ctx, since)
	if err != nil {
		return err
	}
	runs := make(map[string]model.Run, len(recent))
	for _, run := range recent {
		runs[run.ID] = run
	}
	if hub, ok := scheduler.jobs.(interface{ ActiveWorkflowRuns() []WorkflowRunRef }); ok {
		for _, wf := range hub.ActiveWorkflowRuns() {
			if _, exists := runs[wf.RunID]; exists {
				continue
			}
			run, err := scheduler.database().Run(ctx, wf.RunID)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if run.WorkflowName == wf.WorkflowName && (run.Status == model.RunInterrupted || (run.Status == model.RunFailed && strings.HasPrefix(run.Error, "create Workflow:"))) {
				runs[run.ID] = run
			}
		}
	}
	for _, run := range runs {
		repository, err := scheduler.database().Repository(ctx, run.RepoID)
		if err != nil {
			return err
		}
		scheduler.watchFailedCreate(WorkflowRequest{Run: run, Repository: repository, WorkflowName: run.WorkflowName})
	}
	return nil
}

func (database schedulerDatabase) FailedWorkflowCreates(ctx context.Context, since time.Time) ([]model.Run, error) {
	return retryDatabase(ctx, "FailedWorkflowCreates", func(ctx context.Context) ([]model.Run, error) {
		return database.SchedulerStore.FailedWorkflowCreates(ctx, since)
	})
}
