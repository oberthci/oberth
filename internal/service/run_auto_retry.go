package service

import (
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
	"github.com/oberthci/oberth/internal/runprogress"
)

// Call only once a Workflow is terminal, never submitted, or ownership-checked
// cleanup has settled. The store rechecks durable cancellation, publication,
// preflight cleanup, failed-step evidence and lineage atomically with enqueue.
func (scheduler *Scheduler) infrastructureAutoRetry(run model.Run, result JobResult) bool {
	if scheduler.disableInfrastructureAutoRetry || run.RefKind != model.RefBranch || releaseLikeRun(run) || (run.Trigger != "branch" && run.Trigger != "push" && run.Trigger != "schedule" && run.Trigger != "manual") || run.AutoRetryOf != "" || run.AutoRetriedBy != "" || !model.IsInfrastructureRunFailure(result.Status, result.Phase, result.FailedStep) || result.FailedStage != "" || pushretry.PublicationFailure(result.Error) {
		return false
	}
	for _, step := range result.Steps {
		if step.Status == model.StepFailed || step.Status == model.StepTimedOut {
			return false
		}
	}
	if result.TerminalObservation != nil {
		for _, step := range result.TerminalObservation.Steps {
			if step.Status == model.StepFailed || step.Status == model.StepTimedOut {
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
	// Use all retained attempts: a later queued/pass observation cannot erase
	// a real failure. Started or passed work alone is not a test failure.
	for _, event := range events {
		if event.Status == runprogress.StepFailed || event.Status == runprogress.StepTimedOut {
			return false
		}
	}
	return true
}
