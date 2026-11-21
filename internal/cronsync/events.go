package cronsync

import (
	"context"
	"slices"
)

// SyncAll reconciles startup or explicit repair without a server firing loop.
func (c *Syncer) SyncAll(ctx context.Context) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	return c.syncAll(ctx)
}

// SyncRepository reconciles the complete declaration of one repository.
func (c *Syncer) SyncRepository(ctx context.Context, key string) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	return c.syncOne(ctx, key)
}

// RefsChanged skips unrelated refs before waiting or touching native objects.
// Its relevance check reads Git without refreshing (CachedGit).
func (c *Syncer) RefsChanged(ctx context.Context, key string, refs []string) error {
	repo, found, err := c.config.Repository(ctx, key)
	if err != nil {
		return problem(catalogFailure, err)
	}
	if found && !repo.Archived && len(refs) > 0 && !slices.Contains(refs, "refs/heads/"+repo.DefaultBranch) {
		file, err := c.cachedDeclaration(ctx, repo)
		relevant := err != nil
		for _, entry := range file.Entries {
			relevant = relevant || slices.Contains(refs, "refs/heads/"+entry.Branch)
		}
		for _, entry := range file.Refused {
			relevant = relevant || slices.Contains(refs, "refs/heads/"+entry.Branch)
		}
		if !relevant {
			return nil
		}
	}
	return c.SyncRepository(ctx, key)
}
