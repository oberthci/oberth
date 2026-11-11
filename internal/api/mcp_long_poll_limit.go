package api

import (
	"fmt"
	"sync"
)

// MaxLongPollsPerCredential bounds the blocking calls (wait, promote_status,
// run_wait, events_wait) one bearer credential may hold open at once. Each
// waiter re-reads on every recorded event and on its poll interval, so the
// bound keeps one credential from holding the server's readers. Workers that
// share a credential need one waiter each; events_wait covers several runs.
const MaxLongPollsPerCredential = 64

type longPollLimiter struct {
	mu     sync.Mutex
	active map[string]int
	limit  int
}

// acquire reserves a long-poll slot for owner, or reports false at the limit.
func (limiter *longPollLimiter) acquire(owner string) (func(), bool) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if limiter.active == nil {
		limiter.active = map[string]int{}
	}
	limit := limiter.limit
	if limit <= 0 {
		limit = MaxLongPollsPerCredential
	}
	if limiter.active[owner] >= limit {
		return nil, false
	}
	limiter.active[owner]++
	var once sync.Once
	return func() {
		once.Do(func() {
			limiter.mu.Lock()
			defer limiter.mu.Unlock()
			if limiter.active[owner]--; limiter.active[owner] <= 0 {
				delete(limiter.active, owner)
			}
		})
	}, true
}

func (limiter *longPollLimiter) refusal() string {
	limit := limiter.limit
	if limit <= 0 {
		limit = MaxLongPollsPerCredential
	}
	return fmt.Sprintf("this credential already has %d blocking calls open (wait, promote_status, run_wait, events_wait); let one return, or wait for several runs with one events_wait {run_ids}", limit)
}
