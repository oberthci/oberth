package service

import (
	"context"
	"strings"
	"time"

	"k8s.io/utils/clock"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type SchedulingObserver interface {
	ObserveScheduling(context.Context, model.Run) (*model.ExecutionObservation, error)
}

func (s *API) observeScheduling(ctx context.Context, run model.Run, steps []model.StepResult, force bool) (result *model.RunScheduling, reason string) {
	if !run.Status.Active() {
		return nil, ""
	}
	if !force {
		for _, step := range steps {
			if step.Status != model.StepQueued && step.Status != model.StepPending {
				return nil, ""
			}
		}
	}
	c := s.schedulingClock()
	observed := c.Now().UTC()
	result = &model.RunScheduling{ObservedAt: observed, State: "preparing", Message: "Preparing Workflow submission."}
	defer func() {
		result.AgeSeconds = int64(c.Since(observed) / time.Second)
	}()
	if run.Status == model.RunRunning && (run.Reason == store.PolicyRefreshWaitingReason || strings.HasSuffix(run.Reason, "; "+store.PolicyRefreshWaitingReason)) {
		result.State = "waiting"
		result.Message = store.PolicyRefreshWaitingReason + ": waiting for upstream trusted test policy; no publication is allowed."
		return result, "waiting for upstream policy"
	}
	if isPendingCreate(run) {
		result.State = "infra-pending"
		result.Message = "Workflow creation outcome is uncertain; background cleanup is pending."
		if run.Error == store.ExecutorIdentityPendingMessage {
			result.Message = "workflow startup timed out; cleanup pending"
		}
		return result, "workflow create uncertain, cleanup pending"
	}
	if run.ClaimedUnstarted() || run.Status == model.RunWaiting {
		result.State = "waiting"
		result.Message = "Waiting for Workflow submission."
		return result, "starting"
	}
	if run.Phase == "preflight" {
		result.State, result.Message = "preflight", "Running trusted preflight."
		return result, ""
	}
	if s.schedulingObserver == nil {
		result.State = "unavailable"
		return result, "starting"
	}
	if run.WorkflowName == "" {
		result.State = "startup"
		return result, "starting"
	}
	execution, err := s.schedulingObserver.ObserveScheduling(ctx, run)
	if err != nil || execution == nil {
		result.Execution = &model.ExecutionObservation{State: "unavailable", Message: "Execution observation is unavailable."}
		return result, "starting"
	}
	result.Execution = execution
	result.State = execution.State
	result.Message = execution.Message
	return result, ""
}
func (s *API) schedulingClock() clock.Clock {
	if s.waitClock != nil {
		return s.waitClock
	}
	return clock.RealClock{}
}

// Execution display never changes durable status or decides wait completion.
func applyExecutionDisplay(run *model.Run, scheduling *model.RunScheduling) {
	if run.Phase == "publishing" || scheduling == nil || scheduling.Execution == nil {
		return
	}
	if scheduling.Execution.Phase == "Pending" && !strings.HasPrefix(scheduling.Execution.Reason, "SourceSeed") {
		run.DisplayStatus = model.DisplayQueued
		run.DisplayReason = queuedDisplayReason(scheduling)
	}
	if scheduling.Execution.State == "unavailable" && strings.HasPrefix(scheduling.Execution.Message, "status unknown: cluster API unreachable since ") {
		run.DisplayReason = scheduling.Execution.Message
		return
	}
	if strings.HasPrefix(scheduling.Execution.Reason, "SourceSeed") {
		run.DisplayReason = parkPolicyDisplayReason(*run, displayFailureText(scheduling.Execution.Message))
		return
	}
	if scheduling.Execution.Reason == "Suspended" {
		run.DisplayReason = parkPolicyDisplayReason(*run, displayFailureText(scheduling.Execution.Message))
		return
	}
	for _, pod := range scheduling.Execution.Pods {
		if pod.Phase == "Pending" && strings.HasPrefix(pod.Message, "pod pending: ") {
			run.DisplayReason = parkPolicyDisplayReason(*run, displayFailureText(pod.Message))
			return
		}
	}
	if wait := lockWaitReason(scheduling.Execution); wait != "" {
		run.DisplayReason = parkPolicyDisplayReason(*run, wait)
		return
	}
	switch scheduling.Execution.State {
	case "deadline_exceeded", "deadline_elapsed", "terminal_observed":
		// The run is still running until the store says otherwise; the
		// deadline is its reason, not a second status (#1122).
		message := scheduling.Execution.Message
		if message == "" {
			message = "awaiting finalization"
		}
		run.DisplayReason = parkPolicyDisplayReason(*run, displayFailureText(message))
	}
}

// lockWaitReason preserves Argo's wait message for the reason line.
// Controller parallelism retains its existing presentation.
func lockWaitReason(execution *model.ExecutionObservation) string {
	if execution.Message == model.ParallelismWaitMessage {
		return "w" + strings.TrimSuffix(strings.TrimPrefix(model.ParallelismWaitMessage, "W"), ".")
	}
	messages := []string{execution.Message}
	for _, node := range execution.Nodes {
		if node.Phase == "Pending" {
			messages = append(messages, node.Message)
		}
	}
	for _, message := range messages {
		if !strings.HasPrefix(message, "Waiting for ") || !strings.Contains(message, " lock. Lock status: ") {
			continue
		}
		return message
	}
	return ""
}
