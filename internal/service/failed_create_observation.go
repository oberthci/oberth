package service

import (
	"context"
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
		if err := watcher.WatchFailedCreate(scheduler.observationCtx, request); err != nil && scheduler.observationCtx.Err() == nil {
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
