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
		return child.config.CurrentCPU(ctx), nil
	}
	return jobs.config.CurrentCPU(ctx), nil
}
func (jobs *ArgoJobs) AdoptedAdmissionCPU(ctx context.Context, run model.Run) (argoworkflow.CPUAdmission, error) {
	if jobs.routes == nil {
		return jobs.config.CurrentCPU(ctx), nil
	}
	r := jobs.routes
	binding, found, err := r.store.ExecutionTarget(ctx, run.ID, run.WorkflowName)
	if err != nil {
		return r.local.config.CurrentCPU(ctx), err
	}
	if found && binding.Target != "local" {
		if target, ok := r.definitions[binding.Target]; ok {
			return target.Config.CurrentCPU(ctx), nil
		}
		return argoworkflow.CPUAdmission{Pool: "target:" + binding.Target, HoldReason: "CPU admission held: execution target unavailable"}, nil
	}
	if !found {
		for _, target := range r.definitions {
			_, repoID, ref, _, _, _, _ := target.Config.ExecutionPlacement.Identity()
			if run.RepoID == repoID && run.Ref == ref && run.RefKind == model.RefBranch {
				return target.Config.CurrentCPU(ctx), nil
			}
		}
	}
	return r.local.config.CurrentCPU(ctx), nil
}
func (jobs *ArgoJobs) AdmissionCPUPools(ctx context.Context) []argoworkflow.CPUAdmission {
	if jobs.routes == nil {
		return []argoworkflow.CPUAdmission{jobs.config.CurrentCPU(ctx)}
	}
	pools := []argoworkflow.CPUAdmission{jobs.routes.local.config.CurrentCPU(ctx)}
	for _, target := range jobs.routes.definitions {
		pools = append(pools, target.Config.CurrentCPU(ctx))
	}
	return pools
}
