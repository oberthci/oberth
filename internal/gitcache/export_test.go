package gitcache

import (
	"context"
	"errors"
	"strings"
)

func (c *Cache) receiveTransaction(ctx context.Context, repo, path, actor string, receive func() error, handler ReceiveHandler) error {
	if strings.TrimSpace(actor) == "" {
		return errors.New("receive actor is required")
	}
	if receive == nil || handler == nil {
		return errors.New("receive operation and handler are required")
	}
	identity := c.reservationIdentity(repo, path)
	receiveLock := c.receiveLock(path)
	receiveLock.Lock()
	defer receiveLock.Unlock()
	return c.receiveTransactionSerialized(ctx, identity, path, actor, false, nil, receive, handler)
}

func (c *Cache) reserveReceiveLocked(ctx context.Context, identity, path, actor string, admission ReleaseAdmission) (receiveReservation, error) {
	return c.reserveReceiveAtMostLocked(ctx, identity, path, actor, admission, maximumPublicRefs)
}
