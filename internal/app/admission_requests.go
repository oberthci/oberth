package app

import (
	"context"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func (jobs *ArgoJobs) AdmissionRequests(ctx context.Context, request service.WorkflowRequest) (*argoworkflow.PodRequests, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forRequest(ctx, request)
		if err != nil {
			return nil, err
		}
		return child.AdmissionRequests(ctx, request)
	}
	if request.DemandReceipt == nil {
		return nil, nil
	}
	policy := request.DemandReceipt.CPURequestPolicy()
	if policy == nil {
		policy = &argoworkflow.CPURequestPolicy{MaxMilli: jobs.config.StepCPURequestMaxMilli}
	}
	d, err := argojob.SchedulingDemand(request.DemandReceipt.Workflow(), *policy)
	if err != nil || d == nil {
		return nil, nil
	}
	return &argoworkflow.PodRequests{CPUMilli: d.CPUMilli, MemoryBytes: d.MemoryBytes, Pods: d.Pods}, nil
}

// EffectivePriority freezes aging at the durable admission timestamp.
func workflowPriority(run model.Run) model.RunPriority {
	priority, _ := run.EffectivePriority(time.Now())
	return priority
}

func (jobs *ArgoJobs) AdmissionCPU(ctx context.Context, request service.WorkflowRequest) (argoworkflow.CPUAdmission, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forRequest(ctx, request)
		if err != nil {
			return argoworkflow.CPUAdmission{}, err
		}
		return child.config.CPUAdmission, nil
	}
	return jobs.config.CPUAdmission, nil
}
func (jobs *ArgoJobs) AdoptedAdmissionCPU(ctx context.Context, run model.Run) (argoworkflow.CPUAdmission, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forName(ctx, run.WorkflowName, run.ID)
		if err != nil {
			return argoworkflow.CPUAdmission{}, err
		}
		return child.config.CPUAdmission, nil
	}
	return jobs.config.CPUAdmission, nil
}
