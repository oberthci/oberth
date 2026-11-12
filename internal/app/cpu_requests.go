package app

import (
	"context"
	"log"

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
	cpu := argoworkflow.CPURequestPolicy{MaxMilli: jobs.config.StepCPURequestMaxMilli}
	if history, ok := jobs.auditor.(cpuRequestHistory); ok {
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
	return receipt.WithCPURequestPolicy(cpu), nil
}
