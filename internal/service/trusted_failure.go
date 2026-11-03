package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// TrustedPreflightFailure retains the observed Workflow outcome across cleanup.
// It does not replace the journal's receipt or authorize publication.
type TrustedPreflightFailure struct {
	Result JobResult
	Cause  error
}

func (failure *TrustedPreflightFailure) Error() string { return failure.Cause.Error() }
func (failure *TrustedPreflightFailure) Unwrap() error { return failure.Cause }

func (scheduler *Scheduler) finishTrustedPreflightFailure(ctx context.Context, run model.Run, result JobResult, failure error) error {
	index, indexErr := scheduler.logs.BuildIndex(run.ID)
	stepsErr := scheduler.persistSteps(ctx, run.ID, result.Steps, index)
	tail, tailErr := scheduler.failureExcerpt(run.ID, result.FailedBurn, result.FailedStep)
	failure = errors.Join(failure, indexErr, stepsErr, tailErr)
	_, err := scheduler.database().FinishRun(ctx, run.ID, model.RunResult{
		Status: model.RunFailed, Phase: result.Phase, TestedSHA: run.TestedSHA, BaseSHA: run.BaseSHA,
		FailedBurn: result.FailedBurn, FailedStep: result.FailedStep, Error: failure.Error(), FailureTail: string(tail),
	})
	if errors.Is(err, store.ErrInvalidState) {
		scheduler.signals.NotifyRun(run.ID)
		return nil
	}
	if err != nil {
		return errors.Join(failure, fmt.Errorf("finish trusted preflight run: %w", err))
	}
	if run.Trigger == "promotion" {
		promotion, err := scheduler.database().PromotionByRun(ctx, run.ID)
		if err != nil {
			return err
		}
		scheduler.signals.NotifyPromotion(promotion.ID)
	}
	scheduler.signals.NotifyRun(run.ID)
	return nil
}
