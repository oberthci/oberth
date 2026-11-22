package gitcache_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

func TestDemandRefreshFollowsForgeAtBound(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	now := time.Unix(2000000000, 0)
	cfg.Now = func() time.Time { return now }
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Write(t, "new", []byte("new"))
	next := f.CommitFile(t, "new")
	f.Git(t, f.Work, "push", f.Bare, "main")
	f.Git(t, f.Store(), "update-ref", "refs/heads/accepted", f.Commit)
	now = now.Add(gitcache.RefreshInterval - time.Nanosecond)
	assertAdvertisement(t, c, f.Commit)
	now = now.Add(time.Nanosecond)
	assertAdvertisement(t, c, next)
	ref, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/accepted")
	if err != nil || ref.CommitSHA != f.Commit {
		t.Fatal("refresh changed accepted branch", ref, err)
	}
}

func TestPublishedPromotionImmediatelyUpdatesCache(t *testing.T) {
	f := gittest.New(t)
	c := f.Cache(t, func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil })
	f.Write(t, "promoted", []byte("promoted"))
	next := f.CommitFile(t, "promoted")
	f.Git(t, f.Store(), "fetch", f.Work, next)
	p, err := c.PreparePromotion(t.Context(), gitport.PromotionRequest{Repository: gittest.Repository,
		ActionReceipt: "promotion", Source: gitport.Revision{Repository: gittest.Repository, Commit: next},
		Target: gitport.Ref{Name: "refs/heads/main", ObjectSHA: f.Commit, CommitSHA: f.Commit}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Publish(t.Context(), promotionPublication(f, p)); err != nil {
		t.Fatal(err)
	}
	assertAdvertisement(t, c, p.Result.CommitSHA)
	if err = c.GC(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ReadFile(t.Context(), gitport.Revision{Repository: gittest.Repository, Commit: p.Result.CommitSHA}, "promoted"); err != nil {
		t.Fatal("collection lost published source", err)
	}
}

func assertAdvertisement(t *testing.T, c *gitcache.Cache, head string) {
	t.Helper()
	var out bytes.Buffer
	if err := c.Upload(t.Context(), gittest.Repository, gitcache.ServiceIO{Input: io.NopCloser(strings.NewReader("0000")), Output: &out}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), head+" refs/heads/main") {
		t.Fatal("stale advertisement", out.String())
	}
}

func TestDemandRefreshFetchCost(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	now := time.Unix(2000000000, 0)
	cfg.Now = func() time.Time { return now }
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	wall := time.Now //nolint:forbidigo // Measure real transport cost, independent of the injected refresh clock.
	for _, refresh := range []bool{false, true} {
		start := wall()
		for range 5 {
			if refresh {
				now = now.Add(gitcache.RefreshInterval)
			}
			assertAdvertisement(t, c, f.Commit)
		}
		t.Logf("fetch refresh=%t mean=%s (5 native upload sessions)", refresh, wall().Sub(start)/5)
	}
}

func TestDemandRefreshPreservesPendingReceiveInventory(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	now := time.Unix(2000000000, 0)
	cfg.Now = func() time.Time { return now }
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	r := gateReceive(f)
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/pending", NewSHA: f.Commit})
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, io.ErrClosedPipe }
	if err := c.Receive(t.Context(), r); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	before := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/", "refs/tags/")
	f.Write(t, "next", []byte("next"))
	f.CommitFile(t, "forge advance")
	f.Git(t, f.Work, "push", f.Bare, "main")
	now = now.Add(gitcache.RefreshInterval)
	assertAdvertisement(t, c, f.Commit)
	if _, err := c.RemoteRef(t.Context(), gittest.Repository, "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	after := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/", "refs/tags/")
	if before != after {
		t.Fatal("demand read changed pending receive refs", before, after)
	}
}

// A refresh fetch is bounded by the background bound (thirty minutes), not by
// its caller's fifteen-second wait, so a fetch with much to transfer can end.
func TestDemandRefreshBoundsForgeWork(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	now := time.Unix(2000000000, 0)
	cfg.Now = func() time.Time { return now }
	upstream := cfg.Upstream
	var inspect atomic.Bool
	wall := time.Now //nolint:forbidigo // Context deadlines use real time; the cache's freshness clock stays injected.
	cfg.Upstream = func(ctx context.Context, repo string) (gitcache.Remote, error) {
		if inspect.Load() {
			deadline, ok := ctx.Deadline()
			remaining := deadline.Sub(wall())
			if !ok || remaining <= 15*time.Second || remaining > 30*time.Minute {
				t.Error("forge refresh not bounded by the background bound", deadline)
			}
		}
		return upstream(ctx, repo)
	}
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	inspect.Store(true)
	now = now.Add(gitcache.RefreshInterval)
	if err := c.Fetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
}
