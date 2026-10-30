package service

import (
	"context"
	"errors"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
)

// Never infer pre-step from StartedAt: it is weighted admission, not execution.
// Read every retained attempt, not the latest-only display projection. Missing
// log store, missing journal or corrupt/torn progress refuses automatic replay.
func (scheduler *Scheduler) preStepAutoRetry(run model.Run, result JobResult) bool {
	if run.RefKind != model.RefBranch || releaseLikeRun(run) || (run.Trigger != "branch" && run.Trigger != "push" && run.Trigger != "schedule" && run.Trigger != "manual") || run.AutoRetryOf != "" || run.AutoRetriedBy != "" || result.Status != model.RunFailed || (result.Phase != "infrastructure" && result.Phase != "job") || result.FailedStep != "" || result.FailedBurn != "" || result.ObservedStepStart {
		return false
	}
	for _, step := range result.Steps {
		if step.StartedAt != nil || step.FinishedAt != nil || (step.Status != model.StepPending && step.Status != model.StepQueued) {
			return false
		}
	}
	if result.TerminalObservation != nil {
		for _, step := range result.TerminalObservation.Steps {
			if step.Started {
				return false
			}
		}
	}
	if scheduler.logs == nil {
		return false
	}
	events, err := scheduler.logs.StepProgressHistory(run.ID)
	if err != nil {
		return false
	}
	for _, event := range events {
		if event.StartedAt != nil || event.FinishedAt != nil || event.Status != runprogress.StepQueued {
			return false
		}
	}
	return true
}

// Known preparation causes are an allow-list. Mixed policy/invariant errors
// never acquire retry permission from a joined transient error.
func retryablePreCreateCause(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !retryablePreCreateCause(child) {
				return false
			}
		}
		return true
	}
	// Inspect only this layer; errors.As could skip a joined invariant.
	if classified, ok := err.(interface{ InfrastructureFailure() bool }); ok {
		return classified.InfrastructureFailure()
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return retryablePreCreateCause(wrapped.Unwrap())
	}
	return false
}

type checkoutPreparationFailure struct{ cause error }

func (e *checkoutPreparationFailure) Error() string {
	return "checkout run workspace: " + e.cause.Error()
}
func (e *checkoutPreparationFailure) Unwrap() error { return e.cause }
func (e *checkoutPreparationFailure) InfrastructureFailure() bool {
	return retryableCheckoutCause(e.cause)
}

func retryableCheckoutCause(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !retryableCheckoutCause(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return retryableCheckoutCause(wrapped.Unwrap())
	}
	return errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled)
}
