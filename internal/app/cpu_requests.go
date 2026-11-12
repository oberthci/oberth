package app

import (
	"context"
	"encoding/json"
	"log"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type cpuRequestHistory interface {
	CPURequestHistory(context.Context, int64, string) ([]model.StepResourceUsage, error)
}

func (jobs *ArgoJobs) cpuSizedReceipt(ctx context.Context, run model.Run, source *argoworkflow.ResolvedRunSource, policy argoworkflow.Policy) (*argoworkflow.DemandReceipt, error) {
	receipt, err := source.Receipt(policy)
	if err != nil {
		return nil, err
	}
	return receipt.WithCPURequestPolicy(jobs.cpuPolicy(ctx, run, jobs.config, source.Workflow())), nil
}
func (jobs *ArgoJobs) cpuPolicy(ctx context.Context, run model.Run, config argojob.Config, workflow *wfv1.Workflow) argoworkflow.CPURequestPolicy {
	cap := config.StepCPURequestMaxMilli
	if config.CPUCapacity != nil {
		cap = config.CurrentCPU(ctx).StepMaxMilli
	}
	if config.CPUAdmissionDisabled {
		cap = 0
	}
	cpu := argoworkflow.CPURequestPolicy{MaxMilli: cap}
	if history, ok := jobs.auditor.(cpuRequestHistory); ok && !config.CPUAdmissionDisabled {
		class := "branch"
		if run.Release {
			class = "release"
		} else if run.Trigger == "promotion" || run.Trigger == "schedule" {
			class = run.Trigger
		}
		values, err := history.CPURequestHistory(ctx, run.RepoID, class)
		if err != nil {
			log.Printf("CPU request history unavailable for run %s; using capped declarations: %v", run.ID, err)
		} else {
			cpu.Templates = measured.CPURequestPercentile(values)
		}
	}
	// Retired template names cannot grow a new workflow's frozen binding.
	allowed := map[string]bool{}
	if workflow != nil {
		for _, template := range workflow.Spec.Templates {
			allowed[template.Name] = true
		}
	}
	for template := range cpu.Templates {
		if !allowed[template] {
			delete(cpu.Templates, template)
		}
	}
	// Cap otherwise-unlimited static fan-out at the number of largest Pods
	// the pool can fit. Freeze this with the request sizing decision.
	pool := config.CurrentCPU(ctx)
	if workflow != nil && workflow.Spec.Parallelism == nil && pool.Milli > 0 {
		total, _ := argojob.SchedulingDemand(workflow, cpu)
		if total != nil && total.CPUMilli > pool.Milli {
			single := workflow.DeepCopy()
			one := int64(1)
			single.Spec.Parallelism = &one
			largest, _ := argojob.SchedulingDemand(single, cpu)
			if largest != nil && largest.CPUMilli > 0 {
				cpu.Parallelism = min(int64(256), max(int64(1), pool.Milli/largest.CPUMilli))
			}
		}
	}
	body, _ := json.Marshal(cpu)
	if !cpu.Valid() || len(body) > 32<<10 {
		cpu.Templates = nil
	}
	return cpu
}
