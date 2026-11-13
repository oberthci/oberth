package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
)

// finishObservedRun applies the same native verdict and publication protocol
// to the normal observer and a completed Workflow found during Stop settlement.
func (scheduler *Scheduler) finishObservedRun(finalizeCtx context.Context, run model.Run, repository model.Repository, require mutationRequirement, jobResult JobResult, index runlog.Index, stoppedByOberth bool) error {
	checkoutSHA := run.TestedSHA
	if checkoutSHA == "" {
		checkoutSHA = run.SHA
	}
	if !validJobRunStatus(jobResult.Status) {
		jobResult.Status, jobResult.Phase, jobResult.Error = model.RunFailed, "job", "Job returned an invalid terminal status"
	}
	if gateErr := require(finalizeCtx); gateErr != nil {
		return gateErr
	}
	jobResult = scheduler.withResourceFailure(finalizeCtx, run.ID, jobResult)
	if current, lookupErr := scheduler.database().Run(finalizeCtx, run.ID); lookupErr != nil {
		return lookupErr
	} else {
		run = current
		if (current.Cancelled() || stoppedByOberth) && jobResult.Status != model.RunPassed {
			jobResult.Status, jobResult.Phase = model.RunInterrupted, model.PhaseCancelled
			jobResult.Error = "cancelled by oberth: " + strings.TrimPrefix(jobResult.Error, "Workflow stopped by its Workflow spec: ")
			if current.Cancelled() {
				jobResult.Error = strings.Replace(current.Reason, "cancelling by ", "cancelled by ", 1)
			}
		}
	}

	if err := scheduler.persistSteps(finalizeCtx, run.ID, jobResult.Steps, index); err != nil {
		if jobResult.Status == model.RunInterrupted {
			return err
		}
		jobResult = JobResult{Status: model.RunFailed, Phase: "collect", Error: err.Error()}
	}

	promotion, promotionErr := scheduler.promotionForRun(finalizeCtx, run)
	if promotionErr != nil {
		jobResult = JobResult{Status: model.RunFailed, Phase: "promotion", Error: promotionErr.Error()}
	}
	if publish, gateErr := scheduler.publicationGate(finalizeCtx, run.ID, jobResult, require); gateErr != nil {
		return gateErr
	} else if publish {
		publication, beginErr := scheduler.beginRunPublication(finalizeCtx, repository, run, promotion)
		if beginErr != nil {
			jobResult.Status = model.RunFailed
			jobResult.Phase = "publishing"
			jobResult.Error = beginErr.Error()
		} else {
			_, deliveryErr := scheduler.deliverPublicationWithGate(finalizeCtx, publication.ID, require)
			if deliveryErr != nil {
				return fmt.Errorf("publication %s: %w", publication.ID, deliveryErr)
			}
			return nil
		}
	}
	failureTail := ""
	// Interrupted work does not project a CI-red issue. Historical-log loss
	// must not replace its already-established cut verdict with a red collect error.
	if jobResult.Status == model.RunFailed {
		tail, tailErr := scheduler.failureExcerpt(run.ID, jobResult.FailedStage, jobResult.FailedStep)
		if tailErr != nil {
			jobResult.Status = model.RunFailed
			jobResult.Phase = "collect"
			collectionErr := fmt.Errorf("read bounded failure tail: %w", tailErr)
			if jobResult.Error != "" {
				collectionErr = errors.Join(errors.New(jobResult.Error), collectionErr)
			}
			jobResult.Error = collectionErr.Error()
		} else {
			failureTail = string(tail)
		}
	}
	_, err := scheduler.database().FinishRun(finalizeCtx, run.ID, model.RunResult{

		TerminalObservation: jobResult.TerminalObservation, Status: jobResult.Status, Phase: jobResult.Phase, TestedSHA: checkoutSHA, BaseSHA: jobResult.BaseSHA,
		FailureCause: jobResult.FailureCause, FailedStage: jobResult.FailedStage, FailedStep: jobResult.FailedStep, Error: jobResult.Error,
		FailureTail: failureTail,
	})
	if errors.Is(err, store.ErrInvalidState) {
		// A concurrent terminal observation already recorded the verdict.
		scheduler.signals.NotifyRun(run.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("finish run %s: %w", run.ID, err)
	}
	if promotion.ID != "" {
		scheduler.signals.NotifyPromotion(promotion.ID)
	}
	scheduler.signals.NotifyRun(run.ID)
	return nil
}
