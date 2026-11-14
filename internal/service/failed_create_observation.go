package service

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/oberthci/oberth/internal/model"
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

// Restart is a natural event for checking historical create failures again.
// The shared hub catches a late object during one bounded window; no polling
// or background resubmission is introduced.
func (scheduler *Scheduler) rearmFailedCreates(ctx context.Context) error {
	if _, ok := scheduler.jobs.(interface {
		WatchFailedCreate(context.Context, WorkflowRequest) error
	}); !ok {
		return nil
	}
	runs, err := scheduler.database().FailedWorkflowCreates(ctx)
	if err != nil {
		return err
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

func (database schedulerDatabase) FailedWorkflowCreates(ctx context.Context) ([]model.Run, error) {
	return retryDatabase(ctx, "FailedWorkflowCreates", database.SchedulerStore.FailedWorkflowCreates)
}
