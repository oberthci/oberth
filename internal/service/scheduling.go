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

func (service *API) scheduling(ctx context.Context, run model.Run, steps []model.StepResult) *model.RunScheduling {
	scheduling, _ := service.observeScheduling(ctx, run, steps, true)
	return scheduling
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
				reason = store.PolicyRefreshWaitingReason
			}
		}
		result.AgeSeconds = int64(clock.Since(observed) / time.Second)
		if result.Admission != nil && result.Admission.BackfillMessage != "" {
			result.Message += " " + result.Admission.BackfillMessage
		}
		if run.Priority != "" {
			priority, _ := run.EffectivePriority(observed)
			prefix := run.PriorityMessage(observed)
			if priority != model.PriorityNormal && (result.State == "queued" || result.State == "waiting") {
				prefix = fmt.Sprintf("priority %s: next when capacity frees. %s", priority, prefix)
			}
			result.Message = prefix + " " + result.Message
		}
	}()
	if policyWaiting {
		result.State = "waiting"
		result.Message = store.PolicyRefreshWaitingReason + ": waiting for upstream trusted test policy; no publication is allowed."
		return result, reason
	}
	if isPendingCreate(run) {
		result.State = "infra-pending"
		result.Message = "Workflow creation outcome is uncertain; background cleanup is pending."
		if run.Error == store.ExecutorIdentityPendingMessage || run.Error == store.AdmissionStalledPendingMessage {
			result.Message = run.Error
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
	if run.Status == model.RunQueued {
		result.State = "queued"
		if service.scope == nil {
			if observer, ok := service.admission.(interface{ QueuedAdmissionMessage() string }); ok {
				if message := observer.QueuedAdmissionMessage(); message != "" {
					result.Message = message
					return result, message
				}
			}
		}
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
			admission.Position = int(result.QueuePosition)
			result.Admission = &admission
		}
		if result.Admission != nil && service.scope != nil {
			// The position, the capacity and its use, and the disk floor
			// describe every run, in or out of a scoped actor's scope; a full
			// pool of invisible work must not show (#073).
			admission := *result.Admission
			admission.Position, admission.Used, admission.Capacity, admission.DiskFloor = 0, 0, 0, nil
			admission.Runs, admission.MaxRuns = 0, 0
			admission.NumericBudget = nil
			admission.BackfillMessage = ""
			admission.Pressure = admission.Pressure.Public()
			if blocker := service.visibleBlocker(admission.BlockedBy); blocker != nil {
				admission.BlockedBy = blocker
			} else {
				admission.BlockedBy = nil
			}
			result.Admission = &admission
		}
		if result.Admission != nil && service.redactPressure {
			copy := *result.Admission
			copy.Pressure = copy.Pressure.Public()
			copy.BlockedBy = service.visibleBlocker(copy.BlockedBy)
			result.Admission = &copy
		}

		if result.Admission != nil {
			result.State = result.Admission.State
			switch result.Admission.State {
			case "preparing":
				result.Message = "Preparing the run before resource admission."
				return result, reason
			case "waiting":
				result.Message = waitingAdmissionMessage(result.Admission)
				if result.Admission.KubeVirtCapacity || result.Admission.Pressure != nil {
					// This repository's grant is the blocker, independent of
					// shared weight or other repositories' visibility.
					return result, reason
				}
				if service.scope != nil && result.Admission.GroupHolder == "" && result.Admission.GroupAhead == "" {
					// The group waits name runs of the same repository; any
					// other wait says only that capacity is short, unless
					// the blocking run is itself in scope.
					result.Message = "Waiting for capacity."
					if result.Admission.BlockedBy != nil {
						result.Message += " " + blockedBehindMessage(result.Admission.BlockedBy)
					}
				} else if service.scope == nil && result.Admission.Weight > 0 {
					result.Message += fmt.Sprintf(" Need %d weight; %d/%d used; queue position %d.",
						result.Admission.Weight, result.Admission.Used, result.Admission.Capacity, result.Admission.Position)
				}
				return result, reason
			case "admitted":
				if run.StartedAt == nil {
					if service.observeKVMPreflight(ctx, run, result) {
						result.State = "preflight"
						result.Message = "Trusted KVM preflight submitted; awaiting its result and cleanup before workflow submission."
						return result, reason
					}
					result.State = "waiting"
					result.Message = "Resource admission granted; waiting for workflow submission."
					return result, reason
				}
				result.Message = "Resource admission granted; execution progress has not been recorded."
			}
		}
	}
	if run.StartedAt == nil {
		if service.observeKVMPreflight(ctx, run, result) {
			result.State = "preflight"
			result.Message = "Trusted KVM preflight submitted; awaiting its result and cleanup before workflow submission."
			return result, reason
		}
		result.State = "waiting"
		result.Message = "Waiting for workflow submission."
		return result, reason
	}
	if service.observeKVMPreflight(ctx, run, result) {
		result.State = "preflight"
		result.Message = "Trusted KVM preflight submitted; awaiting its result and cleanup before workflow submission."
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

func (service *API) observeKVMPreflight(ctx context.Context, run model.Run, result *model.RunScheduling) bool {
	if service.kvmPreflights == nil || run.WorkflowName != "" || run.Status != model.RunRunning {
		return false
	}
	preflight, err := service.kvmPreflights.KVMPreflight(ctx, run.ID)
	if err != nil || !preflight.Submitted || preflight.Rejected || preflight.Cleaned {
		return false
	}
	now := service.schedulingClock().Now()
	started, expires := preflight.Intent.AdmittedAt, preflight.Intent.Deadline
	remaining := max(int64(0), int64(expires.Sub(now)/time.Second))
	result.Execution = &model.ExecutionObservation{State: "preflight", Deadline: &model.ExecutionDeadline{
		BudgetSeconds: int64(expires.Sub(started) / time.Second), Clock: "preflight_admitted_at", StartedAt: &started, ExpiresAt: &expires, RemainingSeconds: &remaining, Elapsed: !now.Before(expires),
	}}
	return true
}

// waitingAdmissionMessage names the concurrency group and the exact run a
// grouped run waits on (#658). The group is a validated [a-z0-9-] name and the
// runs are server-issued IDs, so neither can carry arbitrary text.
func waitingAdmissionMessage(admission *model.AdmissionObservation) string {

	if admission.KubeVirtCapacity {
		return "KubeVirt capacity"
	}
	if admission.VMCapacity {
		return "Waiting for trusted VM capacity; no weighted resources or concurrency group held."
	}
	switch {
	case admission.DiskFloor != nil && admission.DiskFloor.BelowFloor:
		return fmt.Sprintf("Waiting for disk space: %d bytes free, floor is %d bytes. "+
			"Admission is held until free bytes exceed the configured floor.",
			admission.DiskFloor.FreeBytes, admission.DiskFloor.FloorBytes)
	case admission.GroupHolder != "":
		return fmt.Sprintf("Waiting for concurrency group %q, held by run %s of the same repository; "+
			"runs outside the group are admitted meanwhile.", admission.Group, admission.GroupHolder)
	case admission.GroupAhead != "":
		return fmt.Sprintf("Waiting for concurrency group %q behind run %s of the same repository, "+
			"which is queued ahead in the group.", admission.Group, admission.GroupAhead)
	case admission.Pressure != nil:
		if admission.Weight == 0 && admission.Size == "" {
			return (model.AdmissionBlocker{Pressure: admission.Pressure}).Reason()
		}
		if admission.Pressure.Redacted || admission.Pressure.Avg10Gate {
			return admission.Pressure.Reason()
		}
		return fmt.Sprintf("Deferred: %s pressure %.1f%% (host PSI avg60); bounded by %ds per run.", admission.Pressure.Resource, admission.Pressure.Avg60, admission.Pressure.MaxDeferSeconds)
	case admission.Weight == 0 && admission.BlockedBy != nil && admission.BlockedBy.Size == "" && admission.BlockedBy.RunCeiling:
		return admission.BlockedBy.Reason()
	case admission.HeadOfQueue && admission.BlockedBy != nil && admission.BlockedBy.Backfilling:
		return fmt.Sprintf("At the head of the weighted resource admission queue (%s); %s smaller runs that fit beside it.",
			admission.BlockedBy.Reason(), admission.BlockedBy.Backfill())
	case admission.HeadOfQueue && admission.BlockedBy != nil && admission.BlockedBy.Draining:
		return fmt.Sprintf("At the head of the weighted resource admission queue (%s); %s: later runs require measured p95 backfill that preserves its reserved start.",
			admission.BlockedBy.Reason(), admission.BlockedBy.Backfill())
	case admission.HeadOfQueue && admission.BlockedBy != nil:
		return fmt.Sprintf("At the head of the weighted resource admission queue (%s); every later run waits behind it.",
			admission.BlockedBy.Reason())
	case admission.DeclaredResources != nil:
		if admission.BlockedBy != nil && admission.BlockedBy.RunCeiling {
			return "Waiting for an IO/run concurrency slot."
		}
		return fmt.Sprintf("Waiting for admission reservation (declared requests/VM demand plus helper and memory headroom reserves): %dm CPU, %d memory bytes.", admission.DeclaredResources.CPUMilli, admission.DeclaredResources.MemoryBytes)
	case admission.Weight == 0 && admission.NumericBudget == nil:
		return "Waiting for a workflow slot."
	case admission.BlockedBy != nil:
		return "Waiting in the weighted resource admission queue, " + blockedBehindMessage(admission.BlockedBy)
	default:
		return "Waiting in the weighted resource admission queue."
	}
}

// blockedBehindMessage names the queue-head run every later run waits behind,
// e.g. "blocked behind exampleorg-ebpf/main (XL, needs a full drain)". The
// repository is a registered name and the ref a validated Git ref.
func blockedBehindMessage(blocker *model.AdmissionBlocker) string {
	if backfill := blocker.Backfill(); backfill != "" {
		return fmt.Sprintf("blocked behind %s/%s (%s; %s).", blocker.Repo, blocker.Ref, blocker.Reason(), backfill)
	}
	return fmt.Sprintf("blocked behind %s/%s (%s).", blocker.Repo, blocker.Ref, blocker.Reason())
}

// visibleBlocker hides a blocker a scoped actor may not see (#073): it names
// another repository's run and the shared budget's use.
func (service *API) visibleBlocker(blocker *model.AdmissionBlocker) *model.AdmissionBlocker {
	if blocker == nil {
		return nil
	}
	if service.scope == nil && !service.redactPressure {
		return blocker
	}
	if service.scope != nil && !service.scope.seesRepo(blocker.RepoID) {
		return nil
	}
	scoped := *blocker
	if service.scope != nil {
		scoped.Used, scoped.Capacity = 0, 0
		scoped.NumericBudget = nil
	}
	scoped.Pressure = scoped.Pressure.Public()
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
	switch scheduling.Execution.State {
	case "deadline_exceeded", "deadline_elapsed", "terminal_observed":
		// The run is still running until the store says otherwise; the
		// deadline is its reason, not a second status (#1122).
		run.DisplayReason = scheduling.Message
	}
}

func (service *API) submittedKVMPreflight(ctx context.Context, run model.Run) bool {
	return service.observeKVMPreflight(ctx, run, &model.RunScheduling{})
}
