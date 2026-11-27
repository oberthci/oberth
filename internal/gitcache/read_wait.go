package gitcache

import (
	"context"
	"sync/atomic"
)

type readWaitContext struct{}

// The wait fact belongs to this read, including repair under the repository
// gate. It is never stored with a resident archive or an authorization result.
func (c *Cache) lockReadWait(ctx context.Context, p string) (bool, error) {
	var waited atomic.Bool
	err := c.lockRead(context.WithValue(ctx, readWaitContext{}, &waited), p)
	return waited.Load(), err
}

func noteReadWait(ctx context.Context) {
	if waited, _ := ctx.Value(readWaitContext{}).(*atomic.Bool); waited != nil {
		waited.Store(true)
	}
}
