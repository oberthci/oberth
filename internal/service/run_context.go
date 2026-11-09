package service

import (
	"context"

	"github.com/oberthci/oberth/internal/model"
)

// Lists already carry global queue positions from their database snapshot.
// Detail and summary reads look up the position without polling execution or
// reading a pipeline checkout. Unknown admission demand stays explicit null.
func (service *API) runAdmissionContext(ctx context.Context, run model.Run, lookupPosition bool) model.Run {
	if !run.Status.Active() {
		return run
	}
	if lookupPosition && run.StartedAt == nil && service.scope == nil {
		if queue, ok := service.runs.(queuedRunObserver); ok {
			if position, err := queue.QueuedRunPosition(ctx, run.ID); err == nil {
				run.QueuePosition = position
			}
		}
	}
	if service.scope != nil {
		run.QueuePosition = 0
	}
	return run
}
