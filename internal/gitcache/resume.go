package gitcache

import (
	"context"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// ResumeAncestor checks two exact, already accepted commits in the cache.
func (c *Cache) ResumeAncestor(ctx context.Context, repo, previous, fix string) (bool, error) {
	for _, sha := range []string{previous, fix} {
		if err := (sourcecontract.Revision{Repository: repo, Commit: sha}).Validate(); err != nil {
			return false, err
		}
	}
	p, err := c.path(repo)
	if err != nil {
		return false, err
	}
	return c.reaches(ctx, p, previous, fix)
}
