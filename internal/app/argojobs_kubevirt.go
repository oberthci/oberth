package app

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// PipelineKubeVirt reads declared VMI demand before the scheduler grants any
// resource. The full submission policy still validates the exact manifests.
func (jobs *ArgoJobs) PipelineKubeVirt(ctx context.Context, request service.JobRequest) (service.KubeVirtResources, error) {
	source, err := jobs.readRunSource(request)
	if err != nil {
		return service.KubeVirtResources{}, jobs.noRunPipelineError(err, request)
	}
	kind, err := runKind(request.Run)
	if err != nil {
		return service.KubeVirtResources{}, err
	}
	tier, err := runTrigger(request.Run)
	if err != nil {
		return service.KubeVirtResources{}, err
	}
	fragments, err := loadFragments(ctx, jobs.fragments, source, tier, request.Run.RefKind, request.Run.Trigger)
	if err != nil {
		return service.KubeVirtResources{}, err
	}
	file, err := jobs.runSourceFile(request)
	if err != nil {
		return service.KubeVirtResources{}, err
	}
	sha := request.Run.TestedSHA
	if sha == "" {
		sha = request.Run.SHA
	}
	resolved, err := argoworkflow.ResolveRunSource(source, fragments, argoworkflow.RunSourceBinding{RunID: request.Run.ID, RepositoryID: request.Repository.ID, SHA: sha, File: file, Trigger: kind, ScheduleEntry: request.Run.ScheduleEntry})
	if err != nil {
		return service.KubeVirtResources{}, err
	}
	policy := argoworkflow.Policy{RunnerImagePrefixes: jobs.config.RunnerImagePrefixes, ContainerDiskPrefixes: jobs.config.ContainerDiskPrefixes}
	workflow := resolved.Workflow()
	kvm, err := argoworkflow.DeclaredKVMTemplates(workflow)
	if err != nil {
		return service.KubeVirtResources{}, err
	}
	if !argoworkflow.DeclaresVMIResources(workflow) && len(kvm) == 0 {
		// Even a zero-VM reservation must reject source changes introducing VMs
		// between sizing and submission. Legacy callers without a durable binding
		// keep their context-free behavior and cannot obtain run-aware credits.
		if request.Run.ID == "" || sha == "" {
			return service.KubeVirtResources{}, nil
		}
		receipt, err := resolved.Receipt(policy)
		return service.KubeVirtResources{Receipt: receipt}, err
	}
	if jobs.kubeVirtGrants == nil {
		return service.KubeVirtResources{}, fmt.Errorf("app: repository %s has no KubeVirt grant", request.Repository.Name)
	}
	budget, granted, err := jobs.kubeVirtGrants.ActiveKubeVirtGrant(ctx, request.Repository.ID)
	if err != nil {
		return service.KubeVirtResources{}, fmt.Errorf("app: load KubeVirt grant: %w", err)
	}
	if !granted {
		return service.KubeVirtResources{}, fmt.Errorf("app: repository %s has no KubeVirt grant", request.Repository.Name)
	}
	grant, ceiling := kubeVirtBudget(budget), jobs.config.KubeVirtCeiling
	if ceiling.IsZero() {
		ceiling = argoworkflow.VMIBudgetLimit()
	}
	if err := grant.Validate(); err != nil {
		return service.KubeVirtResources{}, err
	}
	if err := ceiling.Validate(); err != nil {
		return service.KubeVirtResources{}, err
	}
	policy.KubeVirt, policy.VMIBudget, policy.VMICeiling = true, grant, ceiling
	receipt, err := resolved.Receipt(policy)
	if err != nil {
		return service.KubeVirtResources{}, err
	}
	resources := service.KubeVirtResources{Demand: receipt.Summary().Demand, Limit: grant.Within(ceiling).DemandLimit(), Receipt: receipt}
	return resources, resources.Validate()
}

var _ service.PipelineKubeVirtSizer = (*ArgoJobs)(nil)

// AdoptedKubeVirtDemand comes from the verified live Workflow, including work
// admitted under an older or revoked grant. Such work must remain accounted.
func (jobs *ArgoJobs) AdoptedKubeVirtDemand(run model.Run) (argoworkflow.VMIDemand, error) {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	intent, ok := jobs.intents[run.JobName]
	if !ok || intent.runID != run.ID || intent.cancelled || intent.adoption == nil {
		return argoworkflow.VMIDemand{}, fmt.Errorf("app: no adopted resource binding for run %s", run.ID)
	}
	return intent.adoption.KubeVirt, nil
}

var _ service.AdoptedKubeVirtSizer = (*ArgoJobs)(nil)
