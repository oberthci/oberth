package api

import "time"

func (ss *sessions) expire(id string, now func() time.Time) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if v := ss.items[id]; v != nil {
		remaining := sessionIdleTimeout - now().Sub(v.last)
		if remaining > 0 {
			v.timer.Reset(remaining)
		} else {
			ss.removeLocked(id)
		}
	}
}
func (ss *sessions) removeLocked(id string) {
	if v := ss.items[id]; v != nil {
		v.timer.Stop()
		delete(ss.items, id)
	}
}
func (ss *sessions) close() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.closed = true
	for id := range ss.items {
		ss.removeLocked(id)
	}
}
