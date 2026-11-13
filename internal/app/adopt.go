package app

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

// AdoptedResult consumes a terminal startup observation already confirmed live
// by the controller. Running adoptions continue through WaitResumed.
func (jobs *ArgoJobs) AdoptedResult(ctx context.Context, run model.Run) (service.JobResult, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forName(ctx, run.WorkflowName, run.ID)
		if err != nil {
			return service.JobResult{}, err
		}
		return child.AdoptedResult(ctx, run)
	}
	if err := ctx.Err(); err != nil {
		return service.JobResult{}, err
	}
	jobs.mu.Lock()
	intent, ok := jobs.intents[run.WorkflowName]
	jobs.mu.Unlock()
	if !ok || intent.adoption == nil {
		return service.JobResult{}, service.ErrJobNotTerminal
	}
	if intent.runID != run.ID {
		return service.JobResult{}, fmt.Errorf("%w: conflicting startup adoption", service.ErrAdoptionRefused)
	}
	if intent.adoption.Terminal == nil {
		return service.JobResult{}, service.ErrJobNotTerminal
	}
	if jobs.targetClients != nil {
		if err := jobs.targetClients.VerifyResultCustody(ctx); err != nil {
			return service.JobResult{}, err
		}
	}
	defer jobs.forget(run.WorkflowName, run.ID)
	result := jobs.result(*intent.adoption.Terminal)
	if reason := jobs.collectArtifacts(context.WithoutCancel(ctx), run.WorkflowName, intent.runID, intent.trigger); reason != "" {
		jobs.recordArtifactFailure(context.WithoutCancel(ctx), intent.runID, reason)
		if reason == model.ArtifactCollectionDeadlineReason {
			result.Error = model.WithArtifactCollectionDeadline(result.Error)
		}
	}
	return result, nil
}
