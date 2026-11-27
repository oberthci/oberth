package stateport

import (
	"strings"
	"sync"
)

// ChangeScope selects wake hints, never event data or authority. Run observers
// also register repository invalidations, without observing its other runs.
type ChangeScope struct {
	Repositories, Runs, Upstreams, Invalidations []string
	All                                          bool
}

// Change identifies a committed fact. Invalidate includes exact-run readers;
// Authority is reserved for installation-wide authority changes.
type Change struct {
	Repository, Run, Upstream string
	Invalidate, Authority     bool
	Broadcast                 bool
}

// Subscription keeps one pending hint through any number of commits. Readers
// subscribe before their first read and keep the registration during rereads.
type Subscription struct {
	C     <-chan struct{}
	Close func()
}

// ChangeHub routes committed hints without blocking writers. Its zero value is
// ready to use. Empty keys disappear when the last observer leaves.
type ChangeHub struct {
	mu   sync.Mutex
	keys map[string]map[chan struct{}]struct{}
}

func (h *ChangeHub) Subscribe(scope ChangeScope) Subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.keys == nil {
		h.keys = make(map[string]map[chan struct{}]struct{})
	}
	c := make(chan struct{}, 1)
	keys := scope.keys()
	for _, key := range keys {
		if h.keys[key] == nil {
			h.keys[key] = make(map[chan struct{}]struct{})
		}
		h.keys[key][c] = struct{}{}
	}
	return Subscription{C: c, Close: func() { h.remove(keys, c) }}
}

func (s ChangeScope) keys() []string {
	keys := []string{"authority", "broadcast"}
	if s.All {
		keys = append(keys, "all")
	}
	for prefix, values := range map[string][]string{
		"repo:": s.Repositories, "run:": s.Runs,
		"upstream:": s.Upstreams, "invalidate:": s.Invalidations,
	} {
		for _, value := range values {
			keys = append(keys, prefix+value)
		}
	}
	return keys
}

func (h *ChangeHub) remove(keys []string, c chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, key := range keys {
		delete(h.keys[key], c)
		if len(h.keys[key]) == 0 {
			delete(h.keys, key)
		}
	}
}

// Publish is called only after a successful commit. Duplicate keys and mixed
// changes coalesce into the subscriber's single pending hint.
func (h *ChangeHub) Publish(changes []Change) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, change := range changes {
		for _, key := range change.keys() {
			for c := range h.keys[key] {
				select {
				case c <- struct{}{}:
				default:
				}
			}
		}
	}
}

func (c Change) keys() []string {
	keys := []string{"all"}
	if c.Authority {
		keys = append(keys, "authority")
	}
	if c.Broadcast {
		keys = append(keys, "broadcast")
	}
	if c.Repository != "" {
		keys = append(keys, "repo:"+c.Repository)
		c.Upstream, _, _ = strings.Cut(c.Repository, "/")
		if c.Invalidate {
			keys = append(keys, "invalidate:"+c.Repository)
		}
	}
	if c.Run != "" {
		keys = append(keys, "run:"+c.Run)
	}
	if c.Upstream == "global" {
		c.Upstream = ""
	}
	return append(keys, "upstream:"+c.Upstream)
}
