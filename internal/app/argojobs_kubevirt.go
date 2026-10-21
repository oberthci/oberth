package app

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// PipelineKubeVirt reads declared VMI demand before the scheduler grants any
// resource. The full submission policy still validates the exact manifests.
func (jobs *ArgoJobs) PipelineKubeVirt(ctx context.Context, request service.JobRequest) (service.KubeVirtResources, error) {
	trigger, err := runTrigger(request.Run)
	if err != nil {
		return service.KubeVirtResources{}, err
	}
	source, err := readArgoSource(request.SourceDir, trigger)
	if err != nil {
		return service.KubeVirtResources{}, noPipelineError(err, trigger, request.Repository.Name)
	}
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		return service.KubeVirtResources{}, err
	}
	if !argoworkflow.DeclaresVMIResources(workflow) {
		return service.KubeVirtResources{}, nil
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
	demand, err := argoworkflow.DeclaredVMIDemand(workflow)
	if err != nil {
		return service.KubeVirtResources{}, err
	}
	resources := service.KubeVirtResources{Demand: demand, Limit: grant.Within(ceiling).DemandLimit()}
	return resources, resources.Validate()
}

var _ service.PipelineKubeVirtSizer = (*ArgoJobs)(nil)
