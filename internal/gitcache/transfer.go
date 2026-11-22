package gitcache

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// backgroundBound is how long a forge transfer that outlives its callers (a
// first clone, a refresh fetch) may run. It is separate from the caller's
// deadline (a request or a schedule sync allows seconds): thirty minutes
// carries a few gigabytes over a modest link, and the idle budget still ends
// a transfer whose data stops moving.
const backgroundBound = 30 * time.Minute

// outcome is a transfer's result: the public refs it moved and its error.
type outcome struct {
	updates []RefUpdate
	err     error
}

// transfer is one forge transfer running under the cache lifetime; done
// closes when result is final.
type transfer struct {
	done   chan struct{}
	result outcome
	// waiting counts the callers still waiting for the result; left records
	// that a caller stopped waiting before the transfer ended.
	waiting int
	left    bool
	// sending records that the forge began sending the transfer a pack, so a
	// caller that stops waiting can tell a transfer under way from a forge
	// that stalls with an error or nothing.
	sending atomic.Bool
}

// sendingKey carries a transfer's sending flag to the forge commands it runs.
type sendingKey struct{}

// forgeSending is the sending flag of the transfer ctx belongs to, or nil.
func forgeSending(ctx context.Context) *atomic.Bool {
	flag, _ := ctx.Value(sendingKey{}).(*atomic.Bool)
	return flag
}

// ending is what a transfer knows about its callers when its result is final:
// whether one left, and whether none waits to take the result.
type ending struct{ left, unattended bool }

// transfers holds the transfers still running, at most one per key, and lets
// Close wait for them.
type transfers struct {
	mu      sync.Mutex
	running map[string]*transfer
	closed  bool
	wait    sync.WaitGroup
}

// close refuses every later transfer, then waits for the ones that started: a
// transfer starts under mu only while closed is false, so none starts once
// Wait has begun.
func (f *transfers) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.wait.Wait()
}

// joinTransfer counts the caller in on the transfer running under key, or
// starts run under the cache lifetime and the background bound. ended runs
// once the result is final, with what the transfer then knew of its callers.
func (c *Cache) joinTransfer(key string, run func(context.Context) outcome, ended func(context.Context, outcome, ending)) *transfer {
	c.transfers.mu.Lock()
	defer c.transfers.mu.Unlock()
	if t := c.transfers.running[key]; t != nil {
		t.waiting++
		return t
	}
	t := &transfer{done: make(chan struct{}), waiting: 1}
	if c.transfers.closed || c.ctx.Err() != nil {
		t.result.err = unavailable("cache", context.Canceled)
		close(t.done)
		return t
	}
	c.transfers.running[key] = t
	c.transfers.wait.Go(func() {
		ctx, cancel := context.WithTimeout(context.WithValue(c.ctx, sendingKey{}, &t.sending), c.budgets.background)
		defer cancel()
		result := run(ctx)
		c.transfers.mu.Lock()
		delete(c.transfers.running, key)
		end := ending{left: t.left, unattended: t.waiting == 0}
		t.result = result
		c.transfers.mu.Unlock()
		close(t.done)
		ended(ctx, result, end)
	})
	return t
}

// awaitTransfer waits within ctx for t under key. It reports false when ctx
// ended while the transfer still runs: the caller has then left it. Otherwise
// the caller takes the result, and the moved refs go to the first taker only.
func (c *Cache) awaitTransfer(ctx context.Context, key string, t *transfer) (outcome, bool) {
	select {
	case <-t.done:
	case <-ctx.Done():
		if c.leave(key, t) {
			return outcome{}, false
		}
		<-t.done
	}
	c.transfers.mu.Lock()
	defer c.transfers.mu.Unlock()
	result := t.result
	t.result.updates = nil
	return result, true
}

// leave marks that a caller stopped waiting while the transfer still runs. It
// reports false when the transfer had already ended: the caller then takes
// its result like any waiter, and no outcome falls between "continues" and
// what the transfer does when it ends unattended.
func (c *Cache) leave(key string, t *transfer) bool {
	c.transfers.mu.Lock()
	defer c.transfers.mu.Unlock()
	if c.transfers.running[key] != t {
		return false
	}
	t.left = true
	t.waiting--
	return true
}
