package service

import (
	"errors"
	"os"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
)

// Never infer pre-step from StartedAt: it is weighted admission, not execution.
// Read every retained attempt, not the latest-only display projection. Missing
// log store or corrupt/torn progress refuses automatic replay.
func (scheduler *Scheduler) preStepAutoRetry(run model.Run, result JobResult) bool {
	if run.RefKind != model.RefBranch || run.Release || run.Trigger == "promotion" || run.Trigger == "plan" || run.Trigger == "apply" || run.AutoRetryOf != "" || run.AutoRetriedBy != "" || result.Status != model.RunFailed || (result.Phase != "infrastructure" && result.Phase != "job") || result.FailedStep != "" || result.FailedBurn != "" || result.ObservedStepStart {
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
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	for _, event := range events {
		if event.StartedAt != nil || event.FinishedAt != nil || event.Status != runprogress.StepQueued {
			return false
		}
	}
	return true
}
