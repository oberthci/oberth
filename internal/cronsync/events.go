package cronsync

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/schedule"
)

// SyncAll reconciles every repository at startup, without a server firing
// loop. It holds the lock for one repository at a time, so ref notices and
// grants are served between repositories instead of waiting for the whole
// pass; each repository reads a fresh native list, and orphans are pruned at
// the end from a fresh catalogue. Resumes stay refused until the pass has
// gone over every repository.
func (c *Syncer) SyncAll(ctx context.Context) error {
	var repos []Repository
	if err := c.locked(ctx, func(ctx context.Context) error {
		var err error
		repos, _, err = c.passStart(ctx)
		return err
	}); err != nil {
		return err
	}
	var failures []error
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		failures = append(failures, c.locked(ctx, func(ctx context.Context) error { return c.startupStep(ctx, repo) }))
	}
	failures = append(failures, c.locked(ctx, c.passEnd))
	return errors.Join(failures...)
}

// startupStep reconciles one repository of the startup pass from its current
// catalogue record: one removed or replaced (same name, new ID) since the
// pass read the catalogue is left to passEnd's prune, and an archived flag
// set meanwhile is honoured.
func (c *Syncer) startupStep(ctx context.Context, listed Repository) error {
	repo, found, err := c.config.Repository(ctx, listed.Qualified)
	if err != nil {
		return problem(catalogFailure, err)
	}
	if !found || repo.ID != listed.ID {
		return nil
	}
	objects, err := c.list(ctx)
	if err != nil {
		return err
	}
	return c.attempt(ctx, repo, objects)
}

// passEnd prunes the parents of repositories that left the catalogue during
// or before the pass and then lets resumes through.
func (c *Syncer) passEnd(ctx context.Context) error {
	repos, objects, err := c.passStart(ctx)
	if err != nil {
		return err
	}
	if err = c.removeOrphans(ctx, repos, objects); err == nil {
		c.fullSyncFailed = false
	}
	return err
}

func (c *Syncer) locked(ctx context.Context, work func(context.Context) error) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	return work(ctx)
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
// Its relevance check reads Git without refreshing (CachedGit). A tag change
// is relevant to every declared schedule: a parent that consumes source seals
// the tag set at admission, and its children must never load a vanished tag.
func (c *Syncer) RefsChanged(ctx context.Context, key string, refs []string) error {
	repo, found, err := c.config.Repository(ctx, key)
	if err != nil {
		return problem(catalogFailure, err)
	}
	if found && !repo.Archived && len(refs) > 0 && !slices.Contains(refs, "refs/heads/"+repo.DefaultBranch) {
		file, err := c.cachedDeclaration(ctx, repo)
		if err == nil && !relevant(file, refs) {
			return nil
		}
	}
	return c.SyncRepository(ctx, key)
}

// relevant reports whether refs touch a declared entry's branch, or a tag
// while any entry is declared.
func relevant(file schedule.File, refs []string) bool {
	declared := len(file.Entries)+len(file.Refused) > 0
	if declared && slices.ContainsFunc(refs, func(ref string) bool { return strings.HasPrefix(ref, "refs/tags/") }) {
		return true
	}
	for _, entry := range file.Entries {
		if slices.Contains(refs, "refs/heads/"+entry.Branch) {
			return true
		}
	}
	return slices.ContainsFunc(file.Refused, func(entry schedule.Refusal) bool { return slices.Contains(refs, "refs/heads/"+entry.Branch) })
}
