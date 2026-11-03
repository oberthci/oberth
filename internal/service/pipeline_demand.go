package service

import (
	"context"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// PipelineDemandSizer binds resource admission and submission to one resolution.
type PipelineDemandSizer interface {
	PipelineDemandReceipt(context.Context, WorkflowRequest) (*argoworkflow.DemandReceipt, error)
}

func (scheduler *Scheduler) pipelineDemandReceipt(ctx context.Context, request WorkflowRequest) (*argoworkflow.DemandReceipt, error) {
	engine := any(scheduler.jobs)
	if request.Run.Release {
		if _, ok := scheduler.releaseJobs.(PipelineDemandSizer); ok {
			engine = scheduler.releaseJobs
		}
	}
	if sizer, ok := engine.(PipelineDemandSizer); ok {
		return sizer.PipelineDemandReceipt(ctx, request)
	}
	return nil, nil
}
