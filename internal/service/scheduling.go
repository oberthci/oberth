package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/utils/clock"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type SchedulingObserver interface {
	ObserveScheduling(context.Context, model.Run) (*model.ExecutionObservation, error)
}

type AdmissionObserver interface {
	AdmissionSnapshot(int64) *model.AdmissionObservation
}

// admissionBlockerObserver names the run at the head of the admission queue
// that holds back every later run (#1021).
type admissionBlockerObserver interface {
	AdmissionBlocker() *model.AdmissionBlocker
}

type queuedRunObserver interface {
	QueuedRunPosition(context.Context, string) (int64, error)
}

// List rows already carry a position computed over the complete durable queue.
//
// The second result is the display_reason of a run that has not started:
// what it waits for, in plain words (see queuedDisplayReason). The
// observation keeps its full message; the reason is the one line the
// dashboard, CLI and MCP display fields show next to the queued status.
func (service *API) observeScheduling(ctx context.Context, run model.Run, steps []model.StepResult, lookupPosition bool) (result *model.RunScheduling, reason string) {
	run = service.withAdmissionPriority(run)
	if !run.Status.Active() {
		return nil, ""
	}
	for _, step := range steps {
		if step.Status != model.StepPending && step.Status != model.StepQueued && !lookupPosition {
			return nil, ""
		}
	}
	clock := service.schedulingClock()
	observed := clock.Now().UTC()
	result = &model.RunScheduling{ObservedAt: observed, State: "unavailable", Message: "Scheduling observation is unavailable."}
	var blocker *model.AdmissionBlocker
	policyWaiting := run.Status == model.RunRunning && (run.Reason == store.PolicyRefreshWaitingReason || strings.HasSuffix(run.Reason, "; "+store.PolicyRefreshWaitingReason))
	defer func() {
		if model.DisplayRunStatus(run) == model.DisplayQueued {
			if reason == "" {
				reason = queuedDisplayReason(result, blocker)
			}
			if policyWaiting {
				reason = "waiting for upstream policy"
			}
		}
		result.AgeSeconds = int64(clock.Since(observed) / time.Second)
		if run.Priority != "" {
			priority, _ := run.EffectivePriority(observed)
			prefix := run.PriorityMessage(observed)
			if model.DisplayRunStatus(run) == model.DisplayQueued {
				prefix = fmt.Sprintf("Priority %s.", priority)
			}
			if priority != model.PriorityStandard && (result.State == "queued" || result.State == "waiting") {
				prefix = fmt.Sprintf("priority %s: next when a run slot opens. %s", priority, prefix)
			}
			result.Message = prefix + " " + result.Message
		}
	}()
	if policyWaiting {
		result.State = "waiting"
		result.Message = store.PolicyRefreshWaitingReason + ": waiting for upstream trusted test policy; no publication is allowed."
		return result, reason
	}
	if run.Phase == "preflight" {
		result.State = "preflight"
		result.Message = "Retired execution preflight; migrate to a workflow capability grant."
		return result, reason
	}
	if isPendingCreate(run) {
		result.State = "infra-pending"
		result.Message = "Workflow creation outcome is uncertain; background cleanup is pending."
		if run.Error == store.ExecutorIdentityPendingMessage || run.Error == store.AdmissionStalledPendingMessage {
			result.Message = "workflow startup timed out; cleanup pending"
			return result, result.Message
		}
		return result, reason
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if run.StartedAt == nil && service.scope == nil {
		result.QueuePosition = run.QueuePosition
		if lookupPosition {
			if queue, ok := service.runs.(queuedRunObserver); ok {
				if position, err := queue.QueuedRunPosition(ctx, run.ID); err == nil {
					result.QueuePosition = position
				}
			}
		}
	}
	if hold, onHold := service.admissionHoldForRun(run); onHold {
		result.State = "queued"
		result.Message = hold.ShortMessage()
		return result, hold.ShortMessage()
	}
	if run.Status == model.RunQueued {
		result.State = "queued"
		result.Message = "Awaiting scheduler claim; queue position follows priority then FIFO, not an execution estimate."
		if observer, ok := service.admission.(admissionBlockerObserver); ok {
			if blocker = service.visibleBlocker(observer.AdmissionBlocker()); blocker != nil {
				result.Message += " " + blockedBehindMessage(blocker)
			}
		}
		return result, reason
	}
	if service.admission != nil {
		result.Admission = service.admission.AdmissionSnapshot(run.QueueSequence)
		if result.Admission != nil {
			admission := *result.Admission
			if result.QueuePosition > 0 {
				admission.Position = int(result.QueuePosition)
			}
			result.Admission = &admission
		}
		if result.Admission != nil && service.scope != nil {
			// The position, the run count and the disk floor describe every
			// run, in or out of a scoped actor's scope; a full pool of
			// invisible work must not show (#073).
			admission := *result.Admission
			admission.Position, admission.Used, admission.Runs = 0, 0, 0
			if blocker := service.visibleBlocker(admission.BlockedBy); blocker != nil {
				admission.BlockedBy = blocker
			} else {
				admission.BlockedBy = nil
			}
			result.Admission = &admission
		}

		if result.Admission != nil {
			result.State = result.Admission.State
			switch result.Admission.State {
			case "preparing":
				result.Message = "Preparing the run."
				return result, reason
			case "waiting":
				result.Message = waitingAdmissionMessage(result.Admission)
				if service.scope != nil {
					// A scoped wait says only that the run is in line,
					// unless the blocking run is itself in scope.
					result.Message = "In line."
					if result.Admission.BlockedBy != nil {
						result.Message += " " + blockedBehindMessage(result.Admission.BlockedBy)
					}
				}
				return result, reason
			case "admitted":
				if run.StartedAt == nil {
					result.State = "waiting"
					result.Message = "Waiting for workflow submission."
					return result, reason
				}
				result.Message = "Waiting for execution progress."
			}
		}
	}
	if run.StartedAt == nil {
		result.State = "waiting"
		result.Message = "Waiting for workflow submission."
		return result, reason
	}
	if service.schedulingObserver == nil {
		return result, reason
	}
	if run.WorkflowName == "" {
		result.State = "startup"
		result.Message = "No execution object name has been recorded; startup cause is unknown."
		return result, reason
	}
	execution, err := service.schedulingObserver.ObserveScheduling(ctx, run)
	if err != nil || execution == nil {
		result.Execution = &model.ExecutionObservation{State: "unavailable", Message: "Execution observation is unavailable."}
		return result, reason
	}
	result.Execution = execution
	result.State = execution.State
	result.Message = execution.Message
	return result, reason
}

// waitingAdmissionMessage names why a prepared run is not granted yet.
func waitingAdmissionMessage(admission *model.AdmissionObservation) string {
	switch {
	case admission.HeadOfQueue && admission.BlockedBy != nil:
		return fmt.Sprintf("At the head of the run queue (%s); every later run waits behind it.",
			admission.BlockedBy.Reason())
	case admission.BlockedBy != nil:
		return "Waiting in the run queue, " + blockedBehindMessage(admission.BlockedBy)
	default:
		return "Waiting in line."
	}
}

// blockedBehindMessage names the queue-head run every later run waits behind,
// e.g. "blocked behind exampleorg-ebpf/main (waiting for disk space)". The
// repository is a registered name and the ref a validated Git ref.
func blockedBehindMessage(blocker *model.AdmissionBlocker) string {
	return fmt.Sprintf("blocked behind %s/%s (%s).", blocker.Repo, blocker.Ref, blocker.Reason())
}

// visibleBlocker hides a blocker a scoped actor may not see (#073): it names
// another repository's run and the shared budget's use.
func (service *API) visibleBlocker(blocker *model.AdmissionBlocker) *model.AdmissionBlocker {
	if blocker == nil {
		return nil
	}
	if service.scope == nil {
		return blocker
	}
	if service.scope != nil && !service.scope.seesRepo(blocker.RepoID) {
		return nil
	}
	scoped := *blocker
	if service.scope != nil {
		scoped.Used = 0
	}
	return &scoped
}

// One clock keeps observation age and the long-poll deadline in the same domain.
func (service *API) schedulingClock() clock.Clock {
	if service.waitClock != nil {
		return service.waitClock
	}
	return clock.RealClock{}
}

// Execution display never changes durable status or decides wait completion.
func applyExecutionDisplay(run *model.Run, scheduling *model.RunScheduling) {
	if run.Phase == "publishing" || scheduling == nil || scheduling.Execution == nil {
		return
	}
	for _, pod := range scheduling.Execution.Pods {
		if pod.Phase == "Pending" && strings.HasPrefix(pod.Message, "pod pending: ") {
			run.DisplayReason = parkPolicyDisplayReason(*run, displayFailureText(joinRunMessage(priorityPrefix(*run), pod.Message)))
			return
		}
	}
	if wait := lockWaitReason(scheduling.Execution); wait != "" {
		run.DisplayReason = parkPolicyDisplayReason(*run, displayFailureText(joinRunMessage(priorityPrefix(*run), wait)))
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
		run.DisplayReason = parkPolicyDisplayReason(*run, displayFailureText(joinRunMessage(priorityPrefix(*run), message)))
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
