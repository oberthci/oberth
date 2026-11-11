package service

import (
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
	"github.com/oberthci/oberth/internal/runprogress"
)

// infrastructureAutoRetry returns the automatic retry budget for a failed
// branch run, or zero when it must not be retried (#1524). Call only once a
// Workflow is terminal, never submitted, or ownership-checked cleanup has
// settled. The store rechecks durable cancellation, publication, preflight
// cleanup, failed-step evidence and the lineage's spent budget atomically
// with enqueue.
func (scheduler *Scheduler) infrastructureAutoRetry(run model.Run, result JobResult) int {
	if !scheduler.infrastructureAutoRetryEligible(run, result) {
		return 0
	}
	if scheduler.infrastructureAutoRetryBudget <= 0 {
		return model.DefaultInfrastructureAutoRetryBudget
	}
	return scheduler.infrastructureAutoRetryBudget
}

func (scheduler *Scheduler) infrastructureAutoRetryEligible(run model.Run, result JobResult) bool {
	if scheduler.disableInfrastructureAutoRetry || run.RefKind != model.RefBranch || releaseLikeRun(run) || (run.Trigger != "branch" && run.Trigger != "push" && run.Trigger != "schedule" && run.Trigger != "manual") || run.AutoRetriedBy != "" || !model.IsInfrastructureRunFailure(result.Status, result.Phase, result.FailedStep) || result.FailedStage != "" || pushretry.PublicationFailure(result.Error) {
		return false
	}
	// Never for a failure the retry hint classifies as code (#1524).
	failed := run
	failed.Status, failed.Phase, failed.Error, failed.FailedStep, failed.FailedStage = result.Status, result.Phase, result.Error, result.FailedStep, result.FailedStage
	if hint := runRetryHint(failed, nil); hint == nil || hint.Class != model.RetryInfrastructure {
		return false
	}
	eviction := result.Phase == "infrastructure" && result.TerminalObservation.EvictionOnly() && (run.WorkflowName != "" && result.TerminalObservation.WorkflowName == run.WorkflowName)
	deadline := model.IsExecutionDeadlineReason(result.Error)
	if deadline && result.ObservedStepStart {
		return false
	}
	for _, step := range result.Steps {
		if (step.Status == model.StepFailed && (!eviction || !result.TerminalObservation.EvictedAttempt(step.Stage, step.Step, step.StartedAt, step.FinishedAt))) || (deadline && (step.StartedAt != nil || step.LogEnd > step.LogStart || step.Status == model.StepRunning || step.Status == model.StepPassed)) {
			return false
		}
	}
	if result.TerminalObservation != nil {
		for _, step := range result.TerminalObservation.Steps {
			if (step.Status == model.StepFailed && (!eviction || !step.Infrastructure())) || (deadline && (step.Started || step.Status == model.StepRunning || step.Status == model.StepPassed)) {
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
	// a real failure or start. A deadline after execution began may be a hung
	// suite, so it must not receive another full execution window.
	for _, event := range events {
		if (event.Status == runprogress.StepFailed && (!eviction || !result.TerminalObservation.EvictedAttempt(event.Stage, event.Step, event.StartedAt, event.FinishedAt))) || (deadline && (event.StartedAt != nil || event.Status == runprogress.StepRunning || event.Status == runprogress.StepPassed)) {
			return false
		}
	}
	return true
}
