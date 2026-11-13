package service

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// Failure guidance is durable in the existing error field, so list, detail and
// restart reconciliation agree without polling the execution engine per row.
func (s *Scheduler) withResourceFailure(ctx context.Context, runID string, result JobResult) (annotated JobResult) {
	// Resource guidance may replace Error. Interpret the original Workflow
	// failure first, then retain the independent artifact collection note.
	var collectionDeadline bool
	result.Error, collectionDeadline = model.SplitArtifactCollectionDeadline(result.Error)
	if collectionDeadline {
		defer func() { annotated.Error = model.WithArtifactCollectionDeadline(annotated.Error) }()
	}
	// A startup-deadline kill is infrastructure, never the step's deadline.
	if result.Status != model.RunFailed || evictionDisplayReason(model.Run{Phase: result.Phase, Error: result.Error}) != "" || (result.Phase == "infrastructure" && result.FailedStep == "" && model.IsStartupDeadlineReason(result.Error)) {
		return result
	}
	deadline := model.IsExecutionDeadlineReason(result.Error)
	if model.ResourceFailureMessage(runID, result.Steps, deadline) == "" {
		return result
	}
	result.FailureCause = structuredFailureCause(runID, result)
	if message := model.ResourceFailureMessage(runID, result.Steps, deadline); message != "" {
		result.Error = message
	}
	return result
}

// Summaries use the same validated, run-bound termination evidence as detail advice.
func structuredFailureCause(runID string, result JobResult) string {
	for _, step := range result.Steps {
		if step.Status != model.StepFailed && step.Status != model.StepTimedOut {
			continue
		}
		d := step.Termination
		if d == nil || !d.Available || d.Validate() != nil || d.Container == nil || d.Container.RunID != runID || step.RunID != runID {
			continue
		}
		c := d.Container
		reason := step.Stage + "/" + step.Step + " failed"
		if c.Reason == "OOMKilled" {
			reason += " · OOM"
			if c.MemoryLimitBytes != nil {
				reason += " (limit " + bytesText(*c.MemoryLimitBytes) + ")"
			}
			return displayFailureText(reason)
		}
		if step.Status == model.StepTimedOut || c.PodReason == "DeadlineExceeded" {
			reason += " · deadline"
			if c.ActiveDeadlineSeconds != nil {
				reason += " " + shortDeadline(*c.ActiveDeadlineSeconds)
			}
			return displayFailureText(reason)
		}
	}
	return ""
}
func shortDeadline(seconds int64) string {
	if seconds%60 == 0 {
		return fmt.Sprintf("%dm", seconds/60)
	}
	return fmt.Sprintf("%ds", seconds)
}
