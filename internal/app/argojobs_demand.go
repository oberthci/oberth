package app

import (
	"context"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// PipelineDemandReceipt shares one source resolution with legacy VM sizing.
func (jobs *ArgoJobs) PipelineDemandReceipt(ctx context.Context, request service.JobRequest) (*argoworkflow.DemandReceipt, error) {
	resources, err := jobs.PipelineKubeVirt(ctx, request)
	return resources.Receipt, err
}
