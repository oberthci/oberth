package app

import (
	"context"
	"errors"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func (jobs *ArgoJobs) LivePodRequests(ctx context.Context, run model.Run) (*argoworkflow.PodRequests, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forName(ctx, run.WorkflowName, run.ID)
		if err != nil {
			return nil, err
		}
		return child.LivePodRequests(ctx, run)
	}
	if reader, ok := jobs.controller.(interface {
		LivePodRequests(context.Context, string, string) (*argoworkflow.PodRequests, error)
	}); ok {
		return reader.LivePodRequests(ctx, run.WorkflowName, run.ID)
	}
	return nil, errors.New("live CPU requests unavailable")
}
