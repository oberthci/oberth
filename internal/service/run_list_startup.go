package service

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
)

// List polling needs Kubernetes observations only before the first step starts.
// Read local durable results and all progress attempts, so a later queued retry
// cannot erase a previously observed start. Unavailable evidence never causes
// an execution read. Each eligible row has its own deadline, independent of order.
func (service *API) observeRunListStartup(ctx context.Context, run model.Run) *model.RunScheduling {
	if service.schedulingObserver == nil || service.history == nil || run.StartedAt == nil || model.DisplayRunStatus(run) != model.DisplayRunning {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	steps, err := service.history.StepResults(ctx, run.ID)
	if err != nil {
		return nil
	}
	for _, step := range steps {
		if step.StartedAt != nil || step.LogEnd > step.LogStart || step.Status == model.StepRunning || step.Status == model.StepPassed || step.Status == model.StepFailed || step.Status == model.StepTimedOut {
			return nil
		}
	}
	known := len(steps) > 0
	if service.logs != nil {
		events, err := service.logs.StepProgressHistory(run.ID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err == nil {
			known = true
		}
		for _, event := range events {
			if event.StartedAt != nil || event.Status == runprogress.StepRunning || event.Status == runprogress.StepPassed || event.Status == runprogress.StepFailed || event.Status == runprogress.StepTimedOut {
				return nil
			}
		}
	}
	if !known || ctx.Err() != nil {
		return nil
	}
	scheduling, _ := service.observeScheduling(ctx, run, steps, false)
	return scheduling
}
