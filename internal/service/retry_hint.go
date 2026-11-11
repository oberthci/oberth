package service

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
)

// runRetryHint classifies why a run did not pass (#1514) so an agent can
// retry infrastructure failures without reading logs. It reads only the
// run's own durable fields and, for an active publication, its retry
// state; it never changes a status, a label or the reason line. Nil means
// there is nothing to retry: the run passed, is still running without a
// publication retry, or was cancelled.
func runRetryHint(run model.Run, publication *model.Publication) *model.RetryHint {
	if publication != nil && publication.Status == model.PublicationPending && publication.Attempts > 0 {
		hint := &model.RetryHint{Class: model.RetryInfrastructure, Cause: "git_transport", Action: "none: Oberth retries the publication automatically",
			Detail:      "upstream publication is retrying after a transient transport failure",
			Publication: &model.PublicationRetry{Attempt: publication.Attempts, MaxAttempts: pushretry.MaxAttempts, Automatic: true}}
		if !publication.RetryAfter.IsZero() {
			next := publication.RetryAfter.UTC()
			hint.Publication.NextRetryAt = &next
		}
		return hint
	}
	if !run.Status.Terminal() || run.Status == model.RunPassed || run.Phase == "cancelled" {
		return nil
	}
	infrastructure := func(cause, action, detail string) *model.RetryHint {
		return &model.RetryHint{Class: model.RetryInfrastructure, Cause: cause, Action: action, Detail: detail}
	}
	code := func(cause, action, detail string) *model.RetryHint {
		return &model.RetryHint{Class: model.RetryCode, Cause: cause, Action: action, Detail: detail}
	}
	message := run.Error
	lower := strings.ToLower(message)
	if run.AutoRetriedBy != "" {
		return infrastructure("auto_retried", "none: follow auto_retried_by "+run.AutoRetriedBy, "the server already retried this infrastructure failure")
	}
	if pushretry.PublicationFailure(message) || run.Phase == "publishing" {
		for _, rejected := range []string{"non-fast-forward", "fetch first", "permission denied", "authentication failed", "access denied", "pre-receive hook declined", "protected" + " branch", "protected tag"} {
			if strings.Contains(lower, rejected) && !pushretry.Transient(errors.New(message)) {
				return code("upstream_rejected", "inspect the upstream; a retry repeats the rejection", "upstream rejected the publication: "+rejected)
			}
		}
		return infrastructure("git_transport", "run_rerun (retries only the publication)", "every step passed; only the upstream publication failed")
	}
	if reason := evictionDisplayReason(run); reason != "" {
		if strings.Contains(reason, "storage limit") {
			return code("resource_limit", "raise the step's ephemeral-storage limit in git", reason)
		}
		return infrastructure("eviction", "run_retry", reason)
	}
	// A step's own eviction names the step, so evictionDisplayReason (the
	// step-less infrastructure line) does not see it; without this the
	// generic step branch below advised fixing the code.
	switch {
	case strings.Contains(message, "exceeded its declared ephemeral-storage budget"):
		return code("resource_limit", "raise the step's ephemeral-storage limit in git (run_resources)", boundedText(message, 200))
	case run.FailedStep != "" && (message == "Evicted" || strings.HasPrefix(message, "Evicted: ")):
		// The same kubelet message classification as the step-less line.
		reason := evictionDisplayReason(model.Run{Phase: "infrastructure", Error: message})
		if strings.Contains(reason, "storage limit") {
			return code("resource_limit", "raise the step's ephemeral-storage limit in git (run_resources)", reason)
		}
		return infrastructure("eviction", "run_retry", reason)
	}
	switch {
	case strings.Contains(lower, "executor identity"):
		return infrastructure("executor_identity", "run_retry", "executor identity delivery failed before the step ran")
	case strings.Contains(message, "ImagePullBackOff") || strings.Contains(message, "ErrImagePull") || strings.Contains(lower, "pull image") || strings.Contains(lower, "image pull"):
		return infrastructure("image_pull", "run_rerun", "the step image could not be pulled")
	case model.IsStartupDeadlineReason(message):
		return infrastructure("startup_deadline", "run_retry", "the step never started within its startup allowance")
	case strings.HasPrefix(message, "OOM kill at "):
		return code("oom", "raise the step's memory limit in git (run_resources)", "the step exceeded its memory limit")
	}
	if model.IsExecutionDeadlineReason(message) {
		if limit, observed, ok := deadlineRuntime(message); ok && observed < limit*9/10 {
			return infrastructure("deadline_artifact", "run_rerun", "deadline reported well below the step budget (observed "+observed.String()+" of "+limit.String()+")")
		}
		if run.FailedStep == "" && run.FailedStage == "" {
			return infrastructure("deadline", "run_retry", "execution deadline before any step was identified")
		}
		return code("deadline", "inspect run_logs; a hung or slow step needs a fix or a reviewed deadline in git", "the step ran out of its deadline")
	}
	if upstreamConfigurationFailure(message) {
		return code("upstream_configuration", "set the repository's default branch on the forge, then run_retry", boundedText(message, 200))
	}
	if model.IsInfrastructureRunFailure(run.Status, run.Phase, run.FailedStep) {
		return infrastructure("infrastructure", "run_retry", "the run failed outside any step")
	}
	if run.Status == model.RunInterrupted {
		return infrastructure("interrupted", "run_rerun", "the run was interrupted, not failed")
	}
	if run.FailedStep != "" || run.FailedStage != "" {
		return code("step_failed", "read run_logs for the failed step and fix the code", "step "+strings.Trim(run.FailedStage+"/"+run.FailedStep, "/")+" failed")
	}
	return &model.RetryHint{Class: model.RetryUnknown, Cause: "unknown", Action: "read run_get and run_logs", Detail: boundedText(message, 200)}
}

var deadlineRuntimePattern = regexp.MustCompile(`deadline limit (\d+)s; observed runtime ([0-9hms.µn]+)`)

func deadlineRuntime(message string) (time.Duration, time.Duration, bool) {
	match := deadlineRuntimePattern.FindStringSubmatch(message)
	if match == nil {
		return 0, 0, false
	}
	seconds, err := strconv.Atoi(match[1])
	if err != nil || seconds <= 0 {
		return 0, 0, false
	}
	observed, err := time.ParseDuration(match[2])
	if err != nil {
		return 0, 0, false
	}
	return time.Duration(seconds) * time.Second, observed, true
}

// runPublicationRetry returns an active run's publication while Oberth is
// retrying it automatically; nil otherwise.
func (service *API) runPublicationRetry(ctx context.Context, run model.Run) *model.Publication {
	if run.Status.Terminal() || service.promotions == nil {
		return nil
	}
	publications, err := service.promotions.PendingPublications(ctx)
	if err != nil {
		return nil
	}
	for index := range publications {
		if publications[index].RunID == run.ID && publications[index].Attempts > 0 {
			return &publications[index]
		}
	}
	return nil
}
