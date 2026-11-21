package cronsync

import (
	"context"
	"errors"
)

// AuthorityChanged suspends every owned parent before publishing, then re-admits.
func (c *Syncer) AuthorityChanged(ctx context.Context, publish func(context.Context) error) error {
	if err := c.acquire(ctx); err != nil {
		return problem(suspensionUnknown, err)
	}
	defer c.release()
	objects, err := c.list(ctx)
	if err != nil {
		return problem(suspensionUnknown, err)
	}
	var failures []error
	for i := range objects {
		object := &objects[i]
		if id, entry, ok := owned(object); ok {
			c.remember(id, entry, pendingSync, problem(pendingSync, nil))
			failures = append(failures, c.suspend(ctx, object.Name))
		}
	}
	if err := errors.Join(failures...); err != nil {
		return problem(suspensionUnknown, err)
	}
	if err := ctx.Err(); err != nil {
		return problem(suspensionUnknown, err)
	}
	if publish != nil {
		if err := publish(ctx); err != nil {
			return problem(publicationFailed, err)
		}
	}
	return c.syncAll(ctx)
}
