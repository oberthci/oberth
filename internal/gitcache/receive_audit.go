package gitcache

import (
	"context"
	"errors"
)

func (c *Cache) refuseReceive(ctx context.Context, r ReceiveRequest, cause error) error {
	event := ReceiveEvent{Actor: r.Actor, Repository: r.Repository}
	return errors.Join(cause, c.auditRefusal(ctx, event, cause))
}

func (c *Cache) auditRefusal(ctx context.Context, event ReceiveEvent, cause error) error {
	if c.config.Refused == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.budgets.cleanup)
	defer cancel()
	return c.config.Refused(ctx, event, cause)
}
