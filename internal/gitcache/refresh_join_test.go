package gitcache

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A fetch that ends at a pending receive is read in each caller's own mode,
// whoever started it: an ordinary read that joined a forced look is served
// the cache, and a forced look that joined an ordinary read learns why.
func TestPendingReceiveRefusalIsReadPerCaller(t *testing.T) {
	for _, force := range []bool{false, true} {
		c := &Cache{ctx: t.Context(), budgets: defaultGitBudgets(), transfers: &transfers{running: map[string]*transfer{}},
			config: Config{Root: t.TempDir(), Now: func() time.Time { return time.Unix(2000000000, 0) }, Upstream: func(context.Context, string) (Remote, error) { return Remote{}, nil }}}
		p, err := c.path("origin/team/widget")
		if err != nil {
			t.Fatal(err)
		}
		key := p + "/fetch"
		shared := &transfer{done: make(chan struct{}), waiting: 1}
		c.transfers.running[key] = shared
		done := make(chan error, 1)
		go func() {
			_, err := c.refreshQuietly(t.Context(), "origin/team/widget", force)
			done <- err
		}()
		joined(t, c, shared)
		c.transfers.mu.Lock()
		delete(c.transfers.running, key)
		shared.result = outcome{err: refuse("pending receive")}
		c.transfers.mu.Unlock()
		close(shared.done)
		err = <-done
		var refusal *RefusalError
		if force != (errors.As(err, &refusal) && refusal.Field == "pending receive") || !force && err != nil {
			t.Fatal("a pending receive was read in another caller's mode", force, err)
		}
	}
}

// joined waits until a second caller has joined shared.
func joined(t *testing.T, c *Cache, shared *transfer) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		c.transfers.mu.Lock()
		waiting := shared.waiting
		c.transfers.mu.Unlock()
		if waiting == 2 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("the caller did not join the running fetch")
		case <-tick.C:
		}
	}
}
