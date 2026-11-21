package cronsync

import (
	"context"
	"errors"
)

// AuthorityChanged pauses the owned parents of the repository named by key,
// publishes the change, then re-admits that repository. Grants belong to one
// repository, so no other repository's schedules are touched or read.
//
// err means the change was not published: the barrier or the publisher
// failed. unsynced means it was published but the repository's schedules were
// not re-admitted; they stay paused until a later sync of that repository.
func (c *Syncer) AuthorityChanged(ctx context.Context, key string, publish func(context.Context) error) (unsynced, err error) {
	if err := c.acquire(ctx); err != nil {
		return nil, problem(suspensionUnknown, err)
	}
	defer c.release()
	repo, found, err := c.config.Repository(ctx, key)
	if err != nil {
		return nil, problem(suspensionUnknown, err)
	}
	if found {
		if err := c.pauseRepository(ctx, repo.ID); err != nil {
			return nil, problem(suspensionUnknown, err)
		}
	}
	if publish != nil {
		if err := publish(ctx); err != nil {
			return nil, problem(publicationFailed, err)
		}
	}
	if !found {
		return nil, nil
	}
	objects, err := c.list(ctx)
	if err != nil {
		return err, nil
	}
	return c.syncRepository(ctx, repo, objects), nil
}

// pauseRepository suspends every owned parent of one repository and succeeds
// only when all of them are confirmed suspended.
func (c *Syncer) pauseRepository(ctx context.Context, id int64) error {
	objects, err := c.list(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for i := range objects {
		object := &objects[i]
		if owner, entry, ok := owned(object); ok && owner == id {
			c.remember(owner, entry, pendingSync, problem(pendingSync, nil))
			failures = append(failures, c.suspend(ctx, object.Name))
		}
	}
	if err := errors.Join(failures...); err != nil {
		return err
	}
	return ctx.Err()
}
