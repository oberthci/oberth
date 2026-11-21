package gitcache

import "context"

// ForceFetch exposes the same refresh transaction to interruption probes without
// changing their clock or depending on the demand-read freshness interval.
func (c *Cache) ForceFetch(ctx context.Context, repo string) error {
	return c.refresh(ctx, repo, true)
}
