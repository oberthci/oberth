package gitcache

import (
	"context"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// ObservedRef reads the last forge value imported into this cache. It does
// not refresh: a publication lease must precede any new forge observation.
func (c *Cache) ObservedRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	if err := sourcecontract.ValidatePublicRef(ref); err != nil {
		return gitport.Ref{}, err
	}
	p, err := c.path(repo)
	if err != nil {
		return gitport.Ref{}, err
	}
	if err := c.lockRead(ctx, p); err != nil {
		return gitport.Ref{}, err
	}
	defer c.unlock(p)
	head, err := c.resolve(ctx, p, repo, trackingName(ref))
	head.Name = ref
	return head, err
}
