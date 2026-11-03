package gitcache

import (
	"context"
	"fmt"
	"strings"
)

// ReleasePromotionPin removes only the exact local pin for a terminal abandoned
// promotion. It never contacts upstream or changes public refs. The caller must
// commit abandonment before releasing the pin; absence is an idempotent success.
func (c *Cache) ReleasePromotionPin(ctx context.Context, input, target, sha string) error {
	if err := ValidateBranch(target); err != nil {
		return err
	}
	if err := ValidateSHA(sha); err != nil {
		return err
	}
	sha = strings.ToLower(sha)
	_, path, err := c.path(input)
	if err != nil {
		return err
	}
	lock := c.repoLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return err
	}
	defer lock.Unlock()
	if err := c.checkRepositoryActive(ctx, input); err != nil {
		return err
	}
	ref := promotionRefPrefix + target + "/" + sha
	refs, err := c.listRefs(ctx, path, ref)
	if err != nil {
		return err
	}
	current, exists := refs[ref]
	if !exists {
		return nil
	}
	if current != sha {
		return fmt.Errorf("gitcache: abandoned promotion pin has an unexpected object ID")
	}
	return c.deleteRef(ctx, path, ref, sha)
}
