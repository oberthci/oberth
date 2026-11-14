package service

import (
	"context"
	"errors"
	"os"

	"github.com/oberthci/oberth/internal/model"
)

// resumeObservedRun keeps identity delivery, logs, steps and publication shared
// by restart adoption and a Workflow that was stopped after an observation error.
func (scheduler *Scheduler) resumeObservedRun(ctx context.Context, run model.Run, require mutationRequirement, reservation *submissionOwner, stoppedByOberth bool) error {
	defer reservation.cancel()
	if err := require(ctx); err != nil {
		return err
	}
	repository, err := scheduler.database().Repository(ctx, run.RepoID)
	if err != nil {
		return err
	}

	current, err := scheduler.database().Run(ctx, run.ID)
	if err != nil {
		return err
	}
	if current.Cancelled() {
		if err := scheduler.completePendingCancellationsWithGate(ctx, require); err != nil {
			scheduler.deferCreateCleanup(ctx, current, run.WorkflowName, err)
			return nil
		}
	}
	file, events, retained, err := scheduler.logs.Resume(run.ID)
	if errors.Is(err, os.ErrNotExist) {
		file, err = scheduler.logs.Create(run.ID)
		events = nil
		retained = 0
	}
	if err != nil {
		return err
	}
	// A previous owner can stop between Workflow creation and this transition.
	if run.Phase == "waiting" || isPendingCreate(run) {
		run, err = scheduler.database().MarkRunSubmitted(ctx, run.ID)
		if err != nil {
			_ = file.Close()
			return err
		}
	}
	if run.Status == model.RunRunning && run.Phase == "running" {
		// The matching Workflow is adopted and submission is durable. A
		// failed unlink is retried by terminal workspace recovery.
		_ = os.Remove(scheduler.createAdmissionPath(run.ID))
	}
	err = scheduler.observeRun(ctx, run, repository, require, reservation, runObservation{
		file: file, events: events, retained: retained, adopted: true, stopRequested: stoppedByOberth,
	})
	cleanupCtx, cancel := boundedWorkspaceCleanupContext(ctx)
	defer cancel()
	current, lookupErr := scheduler.database().Run(cleanupCtx, run.ID)
	if lookupErr != nil || !current.Status.Terminal() {
		return errors.Join(err, lookupErr)
	}
	cleanupErr := scheduler.workspaces.cleanupRun(cleanupCtx, run.ID)
	if run.Trigger == "promotion" {
		promotion, promotionErr := scheduler.database().PromotionByRun(cleanupCtx, run.ID)
		if promotionErr != nil {
			cleanupErr = errors.Join(cleanupErr, promotionErr)
		} else {
			cleanupErr = errors.Join(cleanupErr, scheduler.workspaces.cleanupPromotion(cleanupCtx, promotion.ID))
		}
	}
	return errors.Join(err, cleanupErr)
}
