package cronsync

import (
	"context"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/schedule"
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
