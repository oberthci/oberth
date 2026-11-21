package cronsync

import (
	"context"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
)

// SetPause projects current audited intent, including when no parent exists.
func (c *Syncer) SetPause(ctx context.Context, repository Repository, entry string) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	name, err := nativebinding.CronName(repository.ID, entry)
	if err != nil {
		return err
	}
	paused, err := c.currentPause(ctx, repository, entry)
	if err != nil {
		return err
	}
	return c.mutate(ctx, name, nil, func(object *wfv1.CronWorkflow) error {
		object.Spec.Suspend = paused || c.failure(repository.ID, entry) != nil
		return nil
	})
}

func (c *Syncer) currentPause(ctx context.Context, repository Repository, entry string) (bool, error) {
	current, found, err := c.config.Repository(ctx, repository.Qualified)
	if err != nil {
		return false, problem(catalogFailure, err)
	}
	if !found || current.ID != repository.ID || current.Qualified != repository.Qualified {
		return false, problem(catalogFailure, nil)
	}
	paused, err := c.config.PauseIntent(ctx, repository.Qualified, entry)
	if err != nil {
		return false, problem(pauseUnavailable, err)
	}
	if !paused && c.fullSyncFailed {
		return false, problem(pendingSync, nil)
	}
	if failure := c.failure(repository.ID, entry); !paused && failure != nil && !publicerr.Permanent(failure.err) {
		return false, problem(pendingSync, nil)
	}
	return paused, nil
}
