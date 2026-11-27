package api

import "sync"

type reservations struct {
	mu     sync.Mutex
	counts map[string]int
}

func (r *reservations) acquire(key string, limit int) (func(), bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts[key] >= limit {
		return nil, false
	}
	r.counts[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.counts[key]--
			if r.counts[key] == 0 {
				delete(r.counts, key)
			}
		})
	}, true
}
