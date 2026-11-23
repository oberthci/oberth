package gitcache

import (
	"context"

	"github.com/oberthci/oberth/internal/gitport"
)

// emptyForge distinguishes bootstrap from lost history using the same refs as
// fetch, even when the last refresh is recent or a receive is pending.
func (c *Cache) emptyForge(ctx context.Context, p string) error {
	if err := c.lockRead(ctx, p); err != nil {
		return err
	}
	defer c.unlock(p)
	tracked, err := c.tracking(ctx, p)
	if err != nil {
		return err
	}
	if err = lostForgeHistory(tracked); err != nil {
		return err
	}
	return &gitport.EmptyRepositoryError{}
}

func lostForgeHistory(tracked map[string]string) error {
	if len(tracked) != 0 {
		return &gitport.ForgeHistoryLostError{}
	}
	return nil
}
