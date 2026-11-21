package api

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"net/http"
	"sync"
	"time"
)

type session struct {
	owner     string
	identity  string
	timer     *time.Timer
	last      time.Time
	resources map[string]bool
	cursor    *int64
}
type sessions struct {
	mu     sync.Mutex
	closed bool
	items  map[string]*session
}

func (ss *sessions) create(c apicontract.Caller, rt Runtime) (string, error) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.closed {
		return "", refusal("restarting")
	}
	now := rt.Now()
	n := 0
	for id, v := range ss.items {
		if now.Sub(v.last) >= sessionIdleTimeout {
			ss.removeLocked(id)
		} else if v.identity == c.Actor.Identity {
			n++
		}
	}
	if n >= sessionsPerIdentity {
		return "", refusal("sessionsIdentity")
	}
	if len(ss.items) >= sessionsPerInstallation {
		return "", refusal("sessionsInstallation")
	}
	id := rt.Nonce()
	if _, exists := ss.items[id]; exists || id == "" {
		return "", refusal("internal")
	}
	ss.items[id] = &session{owner: c.CredentialID, identity: c.Actor.Identity, last: now, resources: map[string]bool{}}
	ss.items[id].timer = time.AfterFunc(sessionIdleTimeout, func() { ss.expire(id, rt.Now) })
	return id, nil
}
func (ss *sessions) lookup(r *http.Request, c apicontract.Caller, now time.Time) (string, *session, error) {
	id := r.Header.Get("Mcp-Session-Id")
	ss.mu.Lock()
	defer ss.mu.Unlock()
	v := ss.items[id]
	if v == nil || v.owner != c.CredentialID || now.Sub(v.last) >= sessionIdleTimeout {
		return "", nil, refusal("session")
	}
	v.last = now
	copy := *v
	copy.resources = map[string]bool{}
	for k, v := range v.resources {
		copy.resources[k] = v
	}
	if v.cursor != nil {
		cursor := *v.cursor
		copy.cursor = &cursor
	}
	return id, &copy, nil
}
func (ss *sessions) remove(id string) { ss.mu.Lock(); ss.removeLocked(id); ss.mu.Unlock() }
func (ss *sessions) update(id string, fn func(*session)) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if v := ss.items[id]; v != nil {
		fn(v)
	}
}
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request, c apicontract.Caller) {
	id, _, err := s.sessions.lookup(r, c, s.config.Runtime.Now())
	if err != nil {
		s.writeError(w, r, err, 404)
		return
	}
	s.sessions.remove(id)
	w.WriteHeader(204)
}

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

func (ss *sessions) resource(id, run string, subscribe bool) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	v := ss.items[id]
	if v == nil {
		return refusal("session")
	}
	if subscribe {
		if !v.resources[run] && len(v.resources) >= resourcesPerSession {
			return refusal("resources")
		}
		v.resources[run] = true
	} else {
		delete(v.resources, run)
	}
	return nil
}
