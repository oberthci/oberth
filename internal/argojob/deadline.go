package argojob

import (
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/model"
)

func workflowDeadlineExceeded(workflow *wfv1.Workflow) bool {
	if workflow.Status.Phase == wfv1.WorkflowSucceeded {
		return false
	}
	if model.IsExecutionDeadlineReason(workflow.Status.Message) {
		return true
	}
	// Only the final attempt of a step can explain the run's failure. A prior
	// timed-out attempt followed by a successful retry must not turn red.
	for _, step := range StepResults(workflow) {
		if terminal(step.Status) && step.Status != "passed" && step.Status != "skipped" && model.IsExecutionDeadlineReason(workflow.Status.Nodes[step.NodeID].Message) {
			return true
		}
	}
	return false
}

func deadlineVerdictVisible(workflow *wfv1.Workflow, result *model.ExecutionObservation) bool {
	return workflow.Status.Phase == wfv1.WorkflowFailed || workflow.Status.Phase == wfv1.WorkflowError ||
		(workflow.Status.Phase != wfv1.WorkflowSucceeded && result.Deadline != nil && result.Deadline.Elapsed)
}

func markDeadlineExceeded(result *model.ExecutionObservation) {
	result.State = "deadline_exceeded"
	result.Reason = "DeadlineExceeded"
	result.Message = model.ExecutionDeadlineMessage + " Durable run finalization may still be pending."
}

func observeWorkflowDeadline(workflow *wfv1.Workflow, result *model.ExecutionObservation, now time.Time) {
	result.ObservedAt = now
	seconds := workflow.Spec.ActiveDeadlineSeconds
	if seconds != nil && *seconds > 0 && *seconds <= int64((1<<63-1)/time.Second) {
		deadline := &model.ExecutionDeadline{BudgetSeconds: *seconds, Clock: "workflow_started_at"}
		if !workflow.CreationTimestamp.IsZero() {
			created := workflow.CreationTimestamp.UTC()
			deadline.CreatedAt = &created
		}
		if !workflow.Status.StartedAt.IsZero() {
			started := workflow.Status.StartedAt.UTC()
			// Match Argo v4.0.8 getWorkflowDeadline, including second truncation.
			expires := started.Truncate(time.Second).Add(time.Duration(*seconds) * time.Second)
			deadline.StartedAt, deadline.ExpiresAt = &started, &expires
			remaining := max(int64(0), int64(expires.Sub(now)/time.Second))
			if expires.After(now) && expires.Sub(now)%time.Second != 0 {
				remaining++
			}
			deadline.RemainingSeconds = &remaining
			deadline.Elapsed = !now.Before(expires)
		}
		result.Deadline = deadline
	}
	if workflow.Status.Phase.Completed() {
		result.State = "terminal_observed"
		result.Message = "Terminal Workflow observed; durable run finalization is pending."
		if !workflow.Status.FinishedAt.IsZero() {
			finished := workflow.Status.FinishedAt.UTC()
			lag := max(int64(0), int64(now.Sub(finished)/time.Second))
			result.FinishedAt, result.ReconciliationLagSeconds = &finished, &lag
		}
	} else if result.Deadline != nil && result.Deadline.Elapsed {
		result.State = "deadline_elapsed"
		result.Message = "Workflow deadline elapsed; terminal execution state has not yet been observed."
	}
	// Clock arithmetic or exit 143 alone is not evidence of a deadline kill.
	if deadlineVerdictVisible(workflow, result) && workflowDeadlineExceeded(workflow) {
		markDeadlineExceeded(result)
	}
}

func currentDeadlineNodes(workflow *wfv1.Workflow) map[string]bool {
	nodes := map[string]bool{}
	for _, step := range StepResults(workflow) {
		if step.NodeID != "" && step.Status != "passed" && step.Status != "skipped" {
			nodes[step.NodeID] = true
		}
	}
	return nodes
}
