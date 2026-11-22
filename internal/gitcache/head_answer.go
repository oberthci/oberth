package gitcache

import (
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/gitport"
)

// headAnswers keeps the default head each forge identity last answered, so a
// caller whose own read of a slow forge does not finish within its wait
// still decides from the forge's answer when it is at most cacheTrustWindow
// old. A read that answers anything else (an absent branch, a refusal)
// removes the kept answer; one that did not reach the forge keeps it.
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
// at now.
func (a *headAnswers) recent(key string, now time.Time) (gitport.Ref, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	kept, ok := a.byKey[key]
	age := now.Sub(kept.at)
	return kept.head, ok && age >= 0 && age <= cacheTrustWindow
}
