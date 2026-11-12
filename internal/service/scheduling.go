package service

import (
	"context"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"k8s.io/utils/clock"
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
	run = s.runAdmissionContext(ctx, run, true)
	c := s.schedulingClock()
	observed := c.Now().UTC()
	result = &model.RunScheduling{ObservedAt: observed, State: "preparing", Message: "Preparing Workflow submission.", QueuePosition: run.QueuePosition}
	defer func() {
		result.AgeSeconds = int64(c.Since(observed) / time.Second)
		if model.DisplayRunStatus(run) == model.DisplayQueued && reason == "" {
			reason = queuedDisplayReason(result)
		}
	}()
	if run.Status == model.RunRunning && (run.Reason == store.PolicyRefreshWaitingReason || strings.HasSuffix(run.Reason, "; "+store.PolicyRefreshWaitingReason)) {
		result.State = "waiting"
		result.Message = store.PolicyRefreshWaitingReason + ": waiting for upstream trusted test policy; no publication is allowed."
		return result, "waiting for upstream policy"
	}
	if isPendingCreate(run) {
		result.State = "infra-pending"
		result.Message = "Workflow creation outcome is uncertain; background cleanup is pending."
		if run.Error == store.ExecutorIdentityPendingMessage || run.Error == store.AdmissionStalledPendingMessage {
			result.Message = "workflow startup timed out; cleanup pending"
		}
		return result, "workflow create uncertain, cleanup pending"
	}
	if run.Status == model.RunQueued {
		result.State = "queued"
		result.Message = "Accepted; preparing Workflow submission."
		return result, "in line"
	}
	if s.schedulingObserver == nil {
		result.State = "unavailable"
		return result, "starting"
	}
	if run.WorkflowName == "" {
		result.State = "startup"
		return result, "starting"
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
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
	if scheduling.Execution.Phase == "Pending" || scheduling.Execution.Message == model.ParallelismWaitMessage {
		run.DisplayStatus = model.DisplayQueued
		run.DisplayReason = queuedDisplayReason(scheduling)
	}
	for _, pod := range scheduling.Execution.Pods {
		if pod.Phase == "Pending" && strings.HasPrefix(pod.Message, "pod pending: ") {
			run.DisplayReason = parkPolicyDisplayReason(*run, displayFailureText(pod.Message))
			return
		}
	}
	if wait := lockWaitReason(scheduling.Execution); wait != "" {
		run.DisplayReason = parkPolicyDisplayReason(*run, displayFailureText(wait))
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

// lockWaitReason turns the fixed-form synchronization wait of the Workflow
// or of one Pending step into the reason line: "waiting for lock <key> (0 of
// 1 free)". The key is the controller's lock name, already bounded to the
// lock-key grammar by the observation. A Workflow the Argo controller
// postponed for its parallelism reads "waiting for an Argo workflow slot
// (controller parallelism)".
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
		if !strings.HasPrefix(message, model.LockWaitMessagePrefix) {
			continue
		}
		return "w" + strings.TrimSuffix(strings.TrimPrefix(message, "W"), ".")
	}
	return ""
}

type queuedRunObserver interface {
	QueuedRunPosition(context.Context, string) (int64, error)
}
