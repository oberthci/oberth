package service

import (
	"context"

	"github.com/oberthci/oberth/internal/model"
)

// Live native state is useful before any local progress exists and after steps
// start (a Workflow or a later step can be suspended). History is not authority
// for Argo's queue or suspend state. Only successful submission enables reads.
func (service *API) observeRunListExecution(ctx context.Context, run model.Run) *model.RunScheduling {
	if service.schedulingObserver == nil || run.StartedAt == nil || run.WorkflowName == "" || model.DisplayRunStatus(run) != model.DisplayRunning {
		return nil
	}
	scheduling, _ := service.observeScheduling(ctx, run, nil, true)
	return scheduling
}
