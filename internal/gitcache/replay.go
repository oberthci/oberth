package gitcache

import (
	"context"
	"errors"
	"os"
)

// Construction may use a short-lived context; accepted work belongs to the
// configured server lifetime or, for a standalone cache, to Close.
func (c *Cache) startDelivery(ctx context.Context) {
	lifetime := c.config.DeliveryContext //nolint:contextcheck // The configured server owns delivery, not the construction request.
	if lifetime == nil {
		lifetime = context.WithoutCancel(ctx)
	}
	c.ctx, c.stopDelivery = context.WithCancel(lifetime)
}

// ReplayPending precedes refresh and preserves stable original attribution.
// Invalid records quarantine their repository by refusing replay and receive.
func (
	c *Cache,
) ReplayPending(
	ctx context.Context,
	deliver func(context.Context, ReceiveEvent) ([]string, error),
) error {
	paths, err := c.repositories()
	if err != nil {
		return err
	}
	var result error
	for _, p := range paths {
		err := c.replayRepository(ctx, p, deliver)
		result = errors.Join(result, err)
	}
	return result
}
func (
	c *Cache,
) replay(
	ctx context.Context,
	p string,
	deliver func(context.Context, ReceiveEvent) ([]string, error),
) (
	[]string,
	error,
) {
	// Accepted work belongs to the server, even if a waiting pusher leaves.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.config.AdmissionTimeout)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel) //nolint:contextcheck // Server shutdown owns accepted delivery; a pusher cannot cancel it.
	defer stop()
	if c.ctx.Err() != nil {
		cancel()
	}
	record, lines, err := c.recoverReceive(ctx, p)
	if os.IsNotExist(err) {
		return lines, nil
	}
	if err != nil {
		return lines, err
	}
	if len(record.Event.Updates) > 0 {
		delivered, deliveryErr := c.deliver(ctx, record.Event, deliver)
		lines = append(lines, delivered...)
		var abandoned *AbandonReceiveError
		if errors.As(deliveryErr, &abandoned) {
			return []string{abandoned.Error()}, c.abandonReceive(ctx, p, record.Event.Repository, record.Event.ID)
		}
		if deliveryErr != nil {
			return lines, deliveryErr
		}
	}
	err = c.finishReceive(ctx, p, record.Event.ID)
	if err == nil && len(record.Event.Updates) > 0 && c.config.RefChanged != nil {
		c.config.RefChanged(ctx, record.Event.Repository, record.Event.Updates)
	}
	return lines, err
}
func (c *Cache) finishReceive(ctx context.Context, p, id string) error {
	// Replay holds the receive slot; the accepted record excludes public writers.
	if err := acquireContext(ctx, c.repositoryGate(p)); err != nil {
		return err
	}
	defer c.unlock(p)
	return c.finishReceiveLocked(ctx, p, id)
}
func (c *Cache) finishReceiveLocked(ctx context.Context, p, id string) error {
	if err := removeReservation(p); err != nil {
		return err
	}
	err := c.release(ctx, p, "receive-"+id)
	if err == nil {
		c.recordReplayProblem(p, nil)
	}
	return err
}

func (
	c *Cache,
) deliver(
	ctx context.Context,
	event ReceiveEvent,
	deliver func(context.Context, ReceiveEvent) ([]string, error),
) (
	[]string,
	error,
) {
	if deliver == nil {
		return nil, refuse("receive consumer")
	}
	// Delivery can repeat after cancellation or a stop; consumers deduplicate by event ID.
	lines, err := deliver(ctx, event)
	if err != nil {
		return nil, &deliveryPendingError{err}
	}
	return lines, nil
}

func (c *Cache) recoverReceive(ctx context.Context, p string) (Reservation, []string, error) {
	r, lines, err := c.lockReceive(ctx, p, false)
	if err != nil {
		return r, lines, err
	}
	defer c.unlock(p)
	if r.Event.ID == "" {
		return r, lines, os.ErrNotExist
	}
	roots := receiveRoots(r.Event.Updates)
	if len(roots) > 0 {
		if err = c.keepRoots(ctx, p, "receive-"+r.Event.ID, keepRoots{objects: roots}); err != nil {
			return r, lines, err
		}
	}
	return r, lines, nil
}

// Applied means changed since reservation and now at the requested new value.
func (c *Cache) actualUpdates(ctx context.Context, p string, record Reservation) ([]RefUpdate, []string, error) {
	current, err := c.refInventory(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	var updates []RefUpdate
	for _, u := range record.Event.Updates {
		now, before := current[u.Ref], record.Before[u.Ref]
		if now == before {
			continue
		}
		if now != u.NewSHA {
			return nil, nil, &receiveRepairError{refuse("receive recovery mismatch")}
		}
		updates = append(updates, u)
	}
	return updates, receiveRoots(updates), nil
}

func receiveRoots(updates []RefUpdate) []string {
	var roots []string
	for _, u := range updates {
		if u.NewSHA != "" {
			roots = append(roots, u.NewSHA)
		}
	}
	return roots
}

// RetryReceive retries delivery of an accepted receive using its original facts.
func (
	c *Cache,
) RetryReceive(
	ctx context.Context,
	repo string,
	deliver func(context.Context, ReceiveEvent) ([]string, error),
) error {
	p, err := c.path(repo)
	if err != nil {
		return err
	}
	if err = acquireContext(ctx, c.repositoryGate(p+"/receive")); err != nil {
		return err
	}
	defer func() { <-c.repositoryGate(p + "/receive") }()
	_, err = c.replay(ctx, p, deliver)
	return err
}

func (
	c *Cache,
) replayRepository(
	ctx context.Context,
	p string,
	deliver func(context.Context, ReceiveEvent) ([]string, error),
) error {
	slot := c.repositoryGate(p + "/receive")
	if err := acquireContext(ctx, slot); err != nil {
		return err
	}
	defer func() { <-slot }()
	_, err := c.replay(ctx, p, deliver)
	c.recordReplayProblem(p, err)
	return err
}
