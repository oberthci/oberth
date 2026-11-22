package gitcache_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// A pin of the pushed repository's own commit trusts the default head the
// cache holds only while the cache was refreshed from the forge within the
// trust window; with an older cache (its forge away) it is refused naming
// that forge, and the forge is never asked during the push.
func TestOwnPinTrustsTheCachedHeadOnlyWithinTheWindow(t *testing.T) {
	if reachable, err := ownPinDuringPush(t, false); err != nil || !reachable {
		t.Fatal("a fresh cache did not decide the own pin", reachable, err)
	}
	reachable, err := ownPinDuringPush(t, true)
	var refusal *publicerr.Error
	if reachable || !errors.As(err, &refusal) || refusal.Reason() != gitcache.ReasonForgeSilent || refusal.Detail != "Forge of "+gittest.Repository+"." {
		t.Fatal("an older cache decided the own pin", reachable, err)
	}
}

// ownPinDuringPush pushes a branch whose admission pins the pushed
// repository's own commit, with the cache's last refresh older than the
// trust window when stale, and returns the pin's outcome.
func ownPinDuringPush(t *testing.T, stale bool) (bool, error) {
	t.Helper()
	x := newPinFixture(t)
	var reachable bool
	var pinErr error
	x.cfg.PreReceive = func(ctx context.Context, _ gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
		_, reachable, pinErr = s.Pin(ctx, sourcecontract.Revision{Repository: gittest.Repository, Commit: x.f.Commit})
		return nil
	}
	x.start(t)
	if stale {
		x.mu.Lock()
		x.now = x.now.Add(gitcache.CacheTrustWindow)
		x.mu.Unlock()
	}
	r := gateReceive(x.f)
	r.AlwaysRun = true
	r.Input = receiveRefInput(t, x.f, gitcache.RefUpdate{Ref: "refs/heads/topic", NewSHA: x.f.Commit})
	r.Output, r.Diagnostics = io.Discard, io.Discard
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	if err := x.c.Receive(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if x.forge.asked(t, "consumer") != 0 {
		t.Fatal("the own pin asked the forge")
	}
	return reachable, pinErr
}
