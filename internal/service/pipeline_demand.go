package service

import (
	"context"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// PipelineSourceResolver pins the source and fragment resolution used for submission.
type PipelineSourceResolver interface {
	ResolvePipelineSource(context.Context, WorkflowRequest) (*argoworkflow.SourceReceipt, error)
}

func (scheduler *Scheduler) resolvePipelineSource(ctx context.Context, request WorkflowRequest) (*argoworkflow.SourceReceipt, error) {
	engine := any(scheduler.jobs)
	if request.Run.Release {
		if _, ok := scheduler.releaseJobs.(PipelineSourceResolver); ok {
			engine = scheduler.releaseJobs
		}
	}
	if sizer, ok := engine.(PipelineSourceResolver); ok {
		return sizer.ResolvePipelineSource(ctx, request)
	}
	return nil, nil
}
