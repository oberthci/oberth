package gitcache

import (
	"context"
)

func (c *Cache) reserveReceiveLocked(ctx context.Context, identity, path, actor string, admission ReleaseAdmission) (receiveReservation, error) {
	return c.reserveReceiveAtMostLocked(ctx, identity, path, actor, admission, maximumPublicRefs)
}
