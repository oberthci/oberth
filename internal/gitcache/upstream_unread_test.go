package gitcache_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

// failingRegistry makes the upstream registration read fail once, on the
// failAt-th read after arm.
type failingRegistry struct {
	mu            sync.Mutex
	armed         bool
	calls, failAt int
}

func (r *failingRegistry) wrap(upstream func(context.Context, string) (gitcache.Remote, error)) func(context.Context, string) (gitcache.Remote, error) {
	return func(ctx context.Context, repo string) (gitcache.Remote, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.armed {
			if r.calls++; r.calls == r.failAt {
				return gitcache.Remote{}, errors.New("registry unavailable")
			}
		}
		return upstream(ctx, repo)
	}
}

func (r *failingRegistry) arm(failAt int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.armed, r.calls, r.failAt = true, 0, failAt
}

func checkUnreadNamed(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "could not be read after the transfer") || strings.Contains(err.Error(), "address changed") {
		t.Fatal("an unreadable upstream was not named as such", err)
	}
}

// A refresh fetch whose upstream registration cannot be read again at its
// end says so, not that the address changed.
func TestUnreadableUpstreamAfterAFetchIsNamed(t *testing.T) {
	x := newFetchFixture(t, transferring)
	registry := &failingRegistry{}
	x.cfg.Upstream = registry.wrap(x.cfg.Upstream)
	c := x.start(t)
	x.advance(t)
	x.slow.release(t)
	registry.arm(2)
	checkUnreadNamed(t, c.Fetch(t.Context(), gittest.Repository))
}

// A first clone whose upstream registration cannot be read again at its end
// says so, not that the address changed.
func TestUnreadableUpstreamAfterAFirstCloneIsNamed(t *testing.T) {
	x := newFetchFixture(t, transferring)
	registry := &failingRegistry{}
	registry.arm(3)
	x.cfg.Upstream = registry.wrap(x.cfg.Upstream)
	c := reviewNew(t, x.cfg)
	checkUnreadNamed(t, c.Init(t.Context(), gittest.Repository))
}
