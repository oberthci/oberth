package gitcache

import (
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/gitport"
)

// headAnswers keeps the default head each forge identity last answered to an
// admission read, so a caller whose own read of a slow forge does not finish
// within its wait still decides from the forge's answer when it is at most
// cacheTrustWindow old and not older than the cache's last refresh. A read
// that answers anything else (an absent branch, a refusal) removes the kept
// answer; one that did not reach the forge keeps it. Whatever moves a store's
// tracking refs (a refresh, a delivery) forgets the store's answers: the
// cache then knows more than they do.
type headAnswers struct {
	mu    sync.Mutex
	byKey map[string]headAnswer
}

type headAnswer struct {
	head gitport.Ref
	at   time.Time
}

func newHeadAnswers() *headAnswers { return &headAnswers{byKey: map[string]headAnswer{}} }

// record keeps or removes key's answer after a head read that ended at at.
func (a *headAnswers) record(key string, head gitport.Ref, err error, at time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case err == nil:
		a.byKey[key] = headAnswer{head: head, at: at}
	case !forgeSlow(err):
		delete(a.byKey, key)
	}
}

// recent returns key's kept answer when it is at most cacheTrustWindow old
// at now and not older than refreshed, the cache's last refresh.
func (a *headAnswers) recent(key string, now, refreshed time.Time) (gitport.Ref, bool) {
	if a == nil {
		return gitport.Ref{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	kept, ok := a.byKey[key]
	age := now.Sub(kept.at)
	return kept.head, ok && age >= 0 && age <= cacheTrustWindow && !kept.at.Before(refreshed)
}

// forget removes the answers kept for the store at p.
func (a *headAnswers) forget(p string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for key := range a.byKey {
		if strings.HasPrefix(key, p+"/") {
			delete(a.byKey, key)
		}
	}
}
