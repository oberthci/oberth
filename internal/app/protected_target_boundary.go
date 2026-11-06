package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

var ErrProtectedTargetUnqualified = errors.New("protected preflight external target is not qualified")

// Ordinary target bindings describe the parent's Workflow and branch authority.
// They cannot authorize its separate protected Workflow. Until protected-phase
// custody is implemented, refuse rather than execute through the local bundle.
// Recovery does not call this gate: existing local obligations keep their owner.
func (jobs *ArgoJobs) checkProtectedTarget(ctx context.Context, job service.WorkflowRequest) error {
	if jobs.routes == nil {
		return nil
	}
	binding, found, err := jobs.routes.store.ExecutionTarget(ctx, job.Run.ID, "")
	if err != nil {
		return err
	}
	if found && binding.Target != "local" {
		return fmt.Errorf("%w: retained target %s has no protected-phase binding", ErrProtectedTargetUnqualified, binding.Target)
	}
	repo := model.CanonicalRepoKey(job.UpstreamName, job.UpstreamOrg, job.Repository.Name)
	for _, target := range jobs.routes.definitions {
		expected, _, _, _, _, _, _ := target.Config.ExecutionPlacement.Identity()
		// A different branch or a credentialed/tag parent cannot obtain local
		// fallback merely because the ordinary target's branch selector differs.
		if expected == repo {
			return fmt.Errorf("%w: repository %s has an ordinary external target but no protected-phase binding", ErrProtectedTargetUnqualified, repo)
		}
	}
	return nil
}
