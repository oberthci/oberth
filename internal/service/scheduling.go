package service

import (
	"context"
	"fmt"
	"time"

	"k8s.io/utils/clock"

	"github.com/oberthci/oberth/internal/model"
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
	return service.observeScheduling(ctx, run, steps, true)
}

// List rows already carry a position computed over the complete durable queue.
func (service *API) observeScheduling(ctx context.Context, run model.Run, steps []model.StepResult, lookupPosition bool) *model.RunScheduling {
	run = service.withAdmissionPriority(run)
	if !run.Status.Active() {
		return nil
	}
	for _, step := range steps {
		if step.Status != model.StepPending && step.Status != model.StepQueued && !lookupPosition {
			return nil
		}
	}
	clock := service.schedulingClock()
	observed := clock.Now().UTC()
	result := &model.RunScheduling{ObservedAt: observed, State: "unavailable", Message: "Scheduling observation is unavailable."}
	defer func() {
		result.AgeSeconds = int64(clock.Since(observed) / time.Second)
		if run.Priority != "" {
			result.Message = run.PriorityMessage(observed) + " " + result.Message
		}
	}()
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
		result.Message = "Awaiting scheduler claim; queue position follows priority then FIFO, not an execution estimate."
		if observer, ok := service.admission.(admissionBlockerObserver); ok {
			if blocker := service.visibleBlocker(observer.AdmissionBlocker()); blocker != nil {
				result.Message += " " + blockedBehindMessage(blocker)
			}
		}
		return result
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
				result.Message = "Preparing the run before resource admission."
				return result
			case "waiting":
				result.Message = waitingAdmissionMessage(result.Admission)
				if result.Admission.KubeVirtCapacity {
					// This repository's grant is the blocker, independent of
					// shared weight or other repositories' visibility.
					return result
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
				return result
			case "admitted":
				if run.StartedAt == nil {
					if service.submittedKVMPreflight(ctx, run) {
						result.State = "preflight"
						result.Message = "Trusted KVM preflight submitted; awaiting its result and cleanup before workflow submission."
						return result
					}
					result.State = "waiting"
					result.Message = "Resource admission granted; waiting for workflow submission."
					return result
				}
				result.Message = "Resource admission granted; execution progress has not been recorded."
			}
		}
	}
	if run.StartedAt == nil {
		if service.submittedKVMPreflight(ctx, run) {
			result.State = "preflight"
			result.Message = "Trusted KVM preflight submitted; awaiting its result and cleanup before workflow submission."
			return result
		}
		result.State = "waiting"
		result.Message = "Waiting for workflow submission."
		return result
	}
	if service.submittedKVMPreflight(ctx, run) {
		result.State = "preflight"
		result.Message = "Trusted KVM preflight submitted; awaiting its result and cleanup before workflow submission."
		return result
	}
	if service.schedulingObserver == nil {
		return result
	}
	if run.JobName == "" {
		result.State = "startup"
		result.Message = "No execution object name has been recorded; startup cause is unknown."
		return result
	}
	execution, err := service.schedulingObserver.ObserveScheduling(ctx, run)
	if err != nil || execution == nil {
		result.Execution = &model.ExecutionObservation{State: "unavailable", Message: "Execution observation is unavailable."}
		return result
	}
	result.Execution = execution
	result.State = execution.State
	result.Message = execution.Message
	return result
}

func (service *API) submittedKVMPreflight(ctx context.Context, run model.Run) bool {
	if service.kvmPreflights == nil || run.JobName != "" || run.Status != model.RunRunning {
		return false
	}
	preflight, err := service.kvmPreflights.KVMPreflight(ctx, run.ID)
	return err == nil && preflight.Submitted && !preflight.Rejected && !preflight.Cleaned
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
	case admission.HeadOfQueue && admission.BlockedBy != nil && admission.BlockedBy.Backfilling:
		return fmt.Sprintf("At the head of the weighted resource admission queue (%s); %s smaller runs that fit beside it.",
			admission.BlockedBy.Reason(), admission.BlockedBy.Backfill())
	case admission.HeadOfQueue && admission.BlockedBy != nil && admission.BlockedBy.Draining:
		return fmt.Sprintf("At the head of the weighted resource admission queue (%s); %s: no later run is admitted until it starts.",
			admission.BlockedBy.Reason(), admission.BlockedBy.Backfill())
	case admission.HeadOfQueue && admission.BlockedBy != nil:
		return fmt.Sprintf("At the head of the weighted resource admission queue (%s); every later run waits behind it.",
			admission.BlockedBy.Reason())
	case admission.BlockedBy != nil:
		return "Waiting in the weighted resource admission queue, " + blockedBehindMessage(admission.BlockedBy)
	default:
		return "Waiting in the weighted resource admission queue."
	}
}

// blockedBehindMessage names the queue-head run every later run waits behind,
// e.g. "blocked behind cloudtaser-ebpf/main (XL, needs a full drain)". The
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
	if service.scope == nil {
		return blocker
	}
	if !service.scope.seesRepo(blocker.RepoID) {
		return nil
	}
	scoped := *blocker
	scoped.Used, scoped.Capacity = 0, 0
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
	if scheduling == nil || scheduling.Execution == nil {
		return
	}
	switch scheduling.Execution.State {
	case "deadline_exceeded":
		run.DisplayStatus = "deadline-exceeded"
		run.DisplayReason = scheduling.Message
	case "deadline_elapsed", "terminal_observed":
		run.DisplayReason = scheduling.Message
	}
}
