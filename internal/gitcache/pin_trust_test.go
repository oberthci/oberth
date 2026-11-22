package gitcache_test

import (
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/publicerr"
)

type trustCase struct {
	name, diagnostic string
	// age moves the clock after the start, which leaves the last refresh
	// RefreshInterval old.
	age time.Duration
	// side pins a commit of another branch, which the cached default head
	// does not reach.
	side    bool
	refusal string
}

const staleAge = gitcache.CacheTrustWindow

// trustCases: the cache decides only for a slow or unreachable forge and a
// cache refreshed from it within the trust window; a refused key or identity
// refuses even with a fresh cache.
func trustCases() []trustCase {
	return []trustCase{
		{name: "fresh cache, silent forge"},
		{name: "stale cache, silent forge", age: staleAge, refusal: gitcache.ReasonForgeSilent},
		{name: "refreshed exactly the window ago", age: gitcache.CacheTrustWindow - gitcache.RefreshInterval},
		{name: "refreshed just over the window ago", age: gitcache.CacheTrustWindow - gitcache.RefreshInterval + time.Nanosecond, refusal: gitcache.ReasonForgeSilent},
		{name: "refresh recorded after now", age: -2 * gitcache.RefreshInterval, refusal: gitcache.ReasonForgeSilent},
		{name: "fresh cache, no server answered", diagnostic: gitssh.ReasonUnanswered},
		{name: "stale cache, no server answered", diagnostic: gitssh.ReasonUnanswered, age: staleAge, refusal: gitcache.ReasonForgeSilent},
		{name: "fresh cache, server went away", diagnostic: gitssh.ReasonLost},
		{name: "fresh cache, transport unavailable", diagnostic: gitssh.ReasonTransport},
		{name: "fresh cache, ended before Git began", diagnostic: gitssh.ReasonEnded, refusal: gitcache.ReasonForgeRefused},
		{name: "fresh cache, command refused", diagnostic: gitssh.ReasonRefused, refusal: gitcache.ReasonForgeRefused},
		{name: "fresh cache, unknown line", diagnostic: "fatal: unexpected answer from the remote", refusal: gitcache.ReasonForgeRefused},
		{name: "fresh cache, identity refused", diagnostic: "Git SSH identity was refused.", refusal: gitcache.ReasonForgeRefused},
		{name: "fresh cache, host key mismatch", diagnostic: gitssh.HostKeyMismatchReason, refusal: gitcache.ReasonForgeRefused},
		{name: "fresh cache, silent forge, commit the cached head does not reach", side: true, refusal: gitcache.ReasonForgeSilent},
	}
}

// prepare starts the fixture for tc and returns the producer commit to pin.
func (x *pinFixture) prepare(t *testing.T, tc trustCase) string {
	t.Helper()
	pinned := x.pipeline.Commit
	if tc.side {
		f := x.pipeline
		f.Git(t, f.Work, "checkout", "-q", "-b", "side")
		f.Write(t, "side", []byte("off the default branch"))
		pinned = f.CommitFile(t, "side commit")
		f.Git(t, f.Work, "push", "-q", f.Bare, "side")
	}
	x.start(t)
	gitcache.SetHeadWait(x.c, 300*time.Millisecond)
	if tc.diagnostic != "" {
		x.forge.failWith(t, tc.diagnostic)
	}
	x.mu.Lock()
	x.now = x.now.Add(tc.age)
	x.mu.Unlock()
	return pinned
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
			at := x.pins()[1]
			at.Commit = x.prepare(t, tc)
			_, reachable, err := x.c.Pin(t.Context(), at)
			if err == nil && !reachable {
				t.Fatal("the pin was not decided")
			}
			if err != nil && reachable {
				t.Fatal("a refused pin was reachable")
			}
			checkTrust(t, tc, at.Repository, err)
		})
	}
}

// A default-branch or tag push reads its own default head under the same
// rule, within the head wait.
func TestDefaultHeadReadFollowsThePinRule(t *testing.T) {
	for _, tc := range trustCases() {
		if tc.side {
			continue
		}
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
		{gitcache.ReasonForgeRefused, gitport.ForgeOperatorRemedy},
	} {
		if publicerr.CheckMessage(m[0], 40) != nil || publicerr.CheckMessage(m[1], 120) != nil {
			t.Fatal("message over its bound", m)
		}
	}
}
