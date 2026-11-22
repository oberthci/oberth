package gitcache_test

import (
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/publicerr"
)

type trustCase struct {
	name, diagnostic string
	stale            bool
	refusal          string
}

// trustCases: the cache decides only for a slow or unreachable forge and a
// cache refreshed from it within the trust window; a refused key or identity
// refuses even with a fresh cache.
func trustCases() []trustCase {
	return []trustCase{
		{name: "fresh cache, silent forge"},
		{name: "stale cache, silent forge", stale: true, refusal: gitcache.ReasonForgeSilent},
		{name: "fresh cache, no server answered", diagnostic: gitssh.ReasonUnanswered},
		{name: "stale cache, no server answered", diagnostic: gitssh.ReasonUnanswered, stale: true, refusal: gitcache.ReasonForgeSilent},
		{name: "fresh cache, identity refused", diagnostic: "Git SSH identity was refused.", refusal: gitcache.ReasonForgeRefused},
		{name: "fresh cache, host key mismatch", diagnostic: gitssh.HostKeyMismatchReason, refusal: gitcache.ReasonForgeRefused},
	}
}

func (x *pinFixture) prepare(t *testing.T, tc trustCase) {
	t.Helper()
	x.start(t)
	gitcache.SetHeadWait(x.c, 300*time.Millisecond)
	if tc.diagnostic != "" {
		x.forge.failWith(t, tc.diagnostic)
	}
	if tc.stale {
		x.mu.Lock()
		x.now = x.now.Add(gitcache.CacheTrustWindow)
		x.mu.Unlock()
	}
}

func checkTrust(t *testing.T, tc trustCase, repo string, err error) {
	t.Helper()
	if tc.refusal == "" {
		if err != nil {
			t.Fatal("the cache did not decide for a slow forge", err)
		}
		return
	}
	var refusal *publicerr.Error
	if !errors.As(err, &refusal) || refusal.Reason() != tc.refusal || refusal.Detail != "Forge of "+repo+"." {
		t.Fatal("wrong refusal", err)
	}
}

func TestPinFallsBackOnlyToARecentCacheForASlowForge(t *testing.T) {
	for _, tc := range trustCases() {
		t.Run(tc.name, func(t *testing.T) {
			x := newPinFixture(t)
			x.prepare(t, tc)
			at := x.pins()[1]
			_, reachable, err := x.c.Pin(t.Context(), at)
			if err == nil && !reachable {
				t.Fatal("the pin was not decided")
			}
			checkTrust(t, tc, at.Repository, err)
		})
	}
}

// A default-branch or tag push reads its own default head under the same
// rule, within the head wait.
func TestDefaultHeadReadFollowsThePinRule(t *testing.T) {
	for _, tc := range trustCases() {
		t.Run(tc.name, func(t *testing.T) {
			x := newPinFixture(t)
			x.prepare(t, tc)
			checkTrust(t, tc, gittest.Repository, x.c.DefaultHead(t.Context(), gittest.Repository))
		})
	}
}

func TestForgeRefusalMessagesFitTheirBounds(t *testing.T) {
	for _, m := range [][2]string{
		{gitcache.ReasonForgeSilent, gitcache.RemedyForgeSilent},
		{gitcache.ReasonForgeRefused, gitcache.RemedyForgeRefused},
	} {
		if publicerr.CheckMessage(m[0], 40) != nil || publicerr.CheckMessage(m[1], 120) != nil {
			t.Fatal("message over its bound", m)
		}
	}
}
