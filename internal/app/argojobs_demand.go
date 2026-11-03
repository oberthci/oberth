package app

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// PipelineDemandReceipt pins the admitted source and fragments before resources
// are reserved. Submission validates that same immutable resolution.
func (jobs *ArgoJobs) PipelineDemandReceipt(ctx context.Context, request service.JobRequest) (*argoworkflow.DemandReceipt, error) {
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
			return nil, fmt.Errorf("app: load workflow capability: %w", err)
		}
		resources, extended := argoworkflow.CapabilityUsage(workflow)
		if capability != nil && (resources || extended) {
			if capability.Namespace != jobs.config.Namespace {
				return nil, fmt.Errorf("app: workflow capability belongs to a different execution namespace")
			}
			policy.Capability = capability
			receipt, err := resolved.Receipt(policy)
			return receipt, err
		}
	}
	return resolved.Receipt(policy)
}

// Restore the server-owned submission binding, never a fresh source analysis.
func (jobs *ArgoJobs) AdoptedPodDemand(run model.Run) (*argoworkflow.PodDemand, error) {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	intent, ok := jobs.intents[run.JobName]
	if !ok || intent.runID != run.ID || intent.adoption == nil {
		return nil, fmt.Errorf("app: no adopted resource binding for run %s", run.ID)
	}
	if !intent.adoption.Pods.Valid() {
		return nil, nil
	}
	d := intent.adoption.Pods
	return &d, nil
}

func (jobs *ArgoJobs) AdoptedResourceFallback(run model.Run) (model.ResourceAdmissionFallback, error) {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	intent, ok := jobs.intents[run.JobName]
	if !ok || intent.runID != run.ID || intent.adoption == nil {
		return "", fmt.Errorf("app: no adopted resource binding for run %s", run.ID)
	}
	return intent.adoption.Fallback, nil
}
