package service

import (
	"context"
	"errors"
	"os"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
)

// prepareReplayTagCheck resolves DB scope before entering the single writer.
// The exact local tag object is rechecked by the authorization callback.
func (service *API) prepareReplayTagCheck(ctx context.Context, run model.Run) (func(model.Run) bool, error) {
	resolver, ok := service.refs.(interface {
		TagObjectSHA(context.Context, string, string) (string, error)
	})
	if !ok {
		return func(model.Run) bool { return false }, nil
	}
	repository, err := service.repositoryByID(ctx, run.RepoID)
	if err != nil {
		return nil, err
	}
	input := service.cacheInputFor(ctx, repository)
	return func(current model.Run) bool {
		sha, err := resolver.TagObjectSHA(ctx, input, current.Ref)
		return err == nil && sameOID(sha, current.SHA)
	}, nil
}

func (service *API) releaseReplayDecision(run model.Run, evidence model.RunReplayEvidence, tagCurrent bool) (model.RunReplayAudit, bool) {
	audit := model.RunReplayAudit{Basis: "unknown"}
	if !evidence.ChainComplete || !evidence.Newest || !tagCurrent || len(evidence.Chain) == 0 {
		return audit, false
	}
	preIntent := true
	for _, attempt := range evidence.Chain {
		audit.RunsChecked++
		audit.PublicationStarted = audit.PublicationStarted || attempt.PublicationStarted
		count, safe := service.releaseAttemptSafe(attempt)
		audit.StepsChecked += count
		if !safe {
			return audit, false
		}
		preIntent = preIntent && attempt.Run.JobName == ""
	}
	audit.Basis = "observed-terminal-no-credential-start"
	if preIntent {
		audit.Basis = "pre-intent"
	}
	return audit, true
}

// These bounded file reads run inside the IMMEDIATE writer transaction for
// atomic evidence/queue/audit decisions; a slow disk can delay other writers.
func (service *API) releaseAttemptSafe(attempt model.RunReplayAttempt) (int, bool) {
	run := attempt.Run
	if !run.Status.Terminal() || attempt.PublicationStarted || run.Phase == "publishing" || service.logs == nil {
		return 0, false
	}
	events, progressErr := service.logs.StepProgressHistory(run.ID)
	if run.JobName == "" {
		return 0, (progressErr == nil || errors.Is(progressErr, os.ErrNotExist)) && len(events) == 0 && len(attempt.Steps) == 0 && run.FailedStep == ""
	}
	observation := attempt.Observation
	if observation == nil || observation.Version != 1 || observation.JobName != run.JobName || observation.Nodes < len(observation.Steps) {
		return 0, false
	}
	if observation.Nodes == 0 && (len(attempt.Steps) > 0 || len(events) > 0 || run.FailedStep != "") {
		return 0, false
	}
	plan, planErr := service.logs.StepPlanRecord(run.ID)
	planned := plan.Steps
	if planErr != nil || plan.Version != runprogress.PlanVersion || len(planned) == 0 {
		return 0, false
	}
	if progressErr != nil {
		if !errors.Is(progressErr, os.ErrNotExist) || observation.Nodes != 0 || len(observation.Steps) != 0 {
			return len(planned), false
		}
	}
	labels := make(map[string]bool, len(planned))
	for _, step := range planned {
		labels[step.Burn+"\x00"+step.Step] = step.Secrets || step.Credentialed
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
			return len(planned), false
		}
	}
	for _, step := range attempt.Steps {
		if !check(step) {
			return len(planned), false
		}
	}
	for _, observed := range observation.Steps {
		result := model.StepResult{Burn: observed.Burn, Step: observed.Step, Status: observed.Status}
		// Started is retained independently of the final/latest attempt status.
		if observed.Started {
			result.Status = model.StepRunning
		}
		if !check(result) {
			return len(planned), false
		}
	}
	if run.FailedStep != "" {
		failed := model.StepResult{Burn: run.FailedBurn, Step: run.FailedStep, Status: model.StepFailed}
		for _, step := range attempt.Steps {
			if step.Burn == run.FailedBurn && step.Step == run.FailedStep {
				failed = step
				break
			}
		}
		if !check(failed) {
			return len(planned), false
		}
	}
	return len(planned), true
}
