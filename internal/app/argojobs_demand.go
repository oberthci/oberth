package app

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// PipelineDemandReceipt admits and pins the exact source and fragment resolution before submission.
func (jobs *ArgoJobs) PipelineDemandReceipt(ctx context.Context, request service.WorkflowRequest) (*argoworkflow.DemandReceipt, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forRequest(ctx, request)
		if err != nil {
			return nil, err
		}
		return child.PipelineDemandReceipt(ctx, request)
	}
	if request.Run.RepoID == 0 {
		request.Run.RepoID = request.Repository.ID
	}
	source, err := jobs.readRunSource(request)
	if err != nil {
		return nil, jobs.noRunPipelineError(err, request)
	}
	kind, err := runKind(request.Run)
	if err != nil {
		return nil, err
	}
	tier, err := runTrigger(request.Run)
	if err != nil {
		return nil, err
	}
	fragments, err := loadFragments(ctx, jobs.fragments, source, tier, request.Run.RefKind, request.Run.Trigger)
	if err != nil {
		return nil, err
	}
	file, err := jobs.runSourceFile(request)
	if err != nil {
		return nil, err
	}
	sha := request.Run.TestedSHA
	if sha == "" {
		sha = request.Run.SHA
	}
	resolved, err := argoworkflow.ResolveRunSource(source, fragments, argoworkflow.RunSourceBinding{RunID: request.Run.ID, RepositoryID: request.Repository.ID, SHA: sha, File: file, Trigger: kind, ScheduleEntry: request.Run.ScheduleEntry})
	if err != nil {
		return nil, err
	}
	policy := argoworkflow.Policy{RunnerImagePrefixes: jobs.config.RunnerImagePrefixes}
	workflow := resolved.Workflow()
	if jobs.capabilityGrants != nil {
		capability, err := jobs.capabilityGrants(ctx, request.Repository.ID)
		if err != nil {
			return nil, err
		}
		resources, extended := argoworkflow.CapabilityUsage(workflow)
		if capability != nil && (resources || extended) {
			if capability.Namespace != jobs.config.Namespace {
				return nil, fmt.Errorf("app: workflow capability belongs to a different execution namespace")
			}
			policy.Capability = capability
		}
	}
	return resolved.Receipt(policy)
}
