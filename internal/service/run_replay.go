package service

import (
	"errors"
	"os"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
)

// releaseReplaySafe is deliberately stricter than the display projection:
// unreadable plan/progress cannot silently erase credential-start evidence.
// DB results and publications are supplied by the enqueue transaction.
func (service *API) releaseReplaySafe(run model.Run, evidence model.RunReplayEvidence) bool {
	if evidence.PublicationStarted || run.Phase == "publishing" || service.logs == nil {
		return false
	}
	planned, planErr := service.logs.StepPlan(run.ID)
	events, progressErr := service.logs.StepProgressHistory(run.ID)
	if progressErr != nil && !errors.Is(progressErr, os.ErrNotExist) {
		return false
	}
	if planErr != nil || len(planned) == 0 {
		// A failure before deterministic Workflow intent cannot have run a step.
		// Missing plans after submission (including historical runs) stay unknown.
		return errors.Is(planErr, os.ErrNotExist) && run.JobName == "" && len(events) == 0 && len(evidence.Steps) == 0
	}
	labels := make(map[string]bool, len(planned))
	for _, step := range planned {
		labels[step.Burn+"\x00"+step.Step] = step.Secrets
	}
	started := func(step model.StepResult) bool {
		if step.StartedAt != nil || step.LogEnd > step.LogStart {
			return true
		}
		if step.Termination != nil && !step.Termination.Available && step.Termination.Unavailable == "not-executed" {
			return false
		}
		switch step.Status {
		case model.StepRunning, model.StepPassed, model.StepFailed, model.StepTimedOut:
			return true
		}
		return false
	}
	check := func(step model.StepResult) bool {
		secret, known := labels[step.Burn+"\x00"+step.Step]
		if !known {
			secret, known = labels[runlog.LegacyRetryBurn(step.Burn)+"\x00"+step.Step]
		}
		return !started(step) || (known && !secret)
	}
	for _, event := range events {
		if !check(progressStepResult(run.ID, event)) {
			return false
		}
	}
	for _, step := range evidence.Steps {
		if !check(step) {
			return false
		}
	}
	if run.FailedStep != "" {
		failed := model.StepResult{Burn: run.FailedBurn, Step: run.FailedStep, Status: model.StepFailed}
		for _, step := range evidence.Steps {
			if step.Burn == run.FailedBurn && step.Step == run.FailedStep {
				failed = step
				break
			}
		}
		if !check(failed) {
			return false
		}
	}
	return true
}
