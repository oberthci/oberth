package service

import (
	"context"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type SchedulingObserver interface {
	ObserveScheduling(context.Context, model.Run) (*model.ExecutionObservation, error)
}

type AdmissionObserver interface {
	AdmissionSnapshot(int64) *model.AdmissionObservation
}

type queuedRunObserver interface {
	QueuedRunPosition(context.Context, string) (int64, error)
}

func (service *API) scheduling(ctx context.Context, run model.Run, steps []model.StepResult) *model.RunScheduling {
	if !run.Status.Active() {
		return nil
	}
	for _, step := range steps {
		if step.Status != model.StepPending && step.Status != model.StepQueued {
			return nil
		}
	}
	observed := time.Now().UTC()
	result := &model.RunScheduling{ObservedAt: observed, State: "unavailable", Message: "Scheduling observation is unavailable."}
	defer func() { result.AgeSeconds = int64(time.Since(observed) / time.Second) }()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if run.Status == model.RunQueued {
		result.State = "queued"
		result.Message = "Awaiting scheduler claim; queue position is accepted order, not an execution estimate."
		if queue, ok := service.runs.(queuedRunObserver); ok {
			if position, err := queue.QueuedRunPosition(ctx, run.ID); err == nil {
				result.QueuePosition = position
			}
		}
		return result
	}
	if service.admission != nil {
		result.Admission = service.admission.AdmissionSnapshot(run.QueueSequence)
		if result.Admission != nil {
			result.State = result.Admission.State
			switch result.Admission.State {
			case "preparing":
				result.Message = "Preparing the run before resource admission."
				return result
			case "waiting":
				result.Message = waitingAdmissionMessage(result.Admission)
				return result
			case "admitted":
				result.Message = "Resource admission granted; execution progress has not been recorded."
			}
		}
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

// waitingAdmissionMessage names the concurrency group and the exact run a
// grouped run waits on (#658). The group is a validated [a-z0-9-] name and the
// runs are server-issued IDs, so neither can carry arbitrary text.
func waitingAdmissionMessage(admission *model.AdmissionObservation) string {
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
	default:
		return "Waiting in the weighted resource admission queue."
	}
}
