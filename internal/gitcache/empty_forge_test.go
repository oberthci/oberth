package gitcache_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

func TestEmptyForgeAcceptsCloneAndRefresh(t *testing.T) {
	f := gittest.New(t)
	f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/main")
	c := reviewNew(t, reviewConfig(t, f))
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal("empty forge cannot receive its first push", err)
	}
	if _, err := os.Stat(filepath.Join(f.Store(), "HEAD")); err != nil {
		t.Fatal("empty clone was not published", err)
	}
	gittest.ExpireRefresh(t, f.Store())
	if err := c.Fetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal("empty refresh blocks the first push", err)
	}
	f.Git(t, f.Bare, "update-ref", "refs/heads/main", f.Commit)
	gittest.ExpireRefresh(t, f.Store())
	if err := c.Fetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal("first commit did not refresh", err)
	}
	f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/main")
	var lost *gitport.ForgeHistoryLostError
	if _, err := c.RemoteRef(t.Context(), gittest.Repository, "HEAD"); !errors.As(err, &lost) {
		t.Fatal("existing cache hid empty forge", err)
	}
}

func TestEmptyForgeRefreshPreservesCachedHistory(t *testing.T) {
	for _, state := range []string{"tracked", "unpublished", "retained"} {
		t.Run(state, func(t *testing.T) {
			checkEmptyForgeCachedHistory(t, state)
		})
	}
}

func TestEmptyForgeLiveReadsRefuseTrackedHistory(t *testing.T) {
	f := gittest.New(t)
	c := f.Cache(t, nil)
	f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/main")
	var lost *gitport.ForgeHistoryLostError
	if err := c.DefaultHead(t.Context(), gittest.Repository); !errors.As(err, &lost) {
		t.Fatal("recent refresh bypassed live gate refusal", err)
	}
	if _, err := c.RemoteRef(t.Context(), gittest.Repository, "refs/heads/main"); !errors.As(err, &lost) {
		t.Fatal("publication read treated lost history as bootstrap", err)
	}
}

func TestEmptyForgePublicationRetryRefusesTrackedHistory(t *testing.T) {
	f := gittest.New(t)
	c := f.Cache(t, nil)
	f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/main")
	before := cacheRefs(t, f)
	var lost *gitport.ForgeHistoryLostError
	intent := publicationFor(f, "refs/heads/main", f.Commit, "")
	if _, err := c.Publish(t.Context(), intent); !errors.As(err, &lost) {
		t.Fatal("saved absent predecessor bypassed lost-history refusal", err)
	}
	if after := cacheRefs(t, f); after != before {
		t.Fatal("refused publication changed the cache", before, after)
	}
	if refs := f.Git(t, f.Bare, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatal("publication retry recreated lost forge history", refs)
	}
}

func checkEmptyForgeCachedHistory(t *testing.T, state string) {
	t.Helper()
	f := gittest.New(t)
	var notices atomic.Int32
	cfg := reviewConfig(t, f)
	cfg.RefChanged = func(context.Context, string, []gitcache.RefUpdate) { notices.Add(1) }
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	ref := "refs/heads/main"
	if state == "retained" {
		ref = "refs/oberth/keep/retained"
	}
	if state != "tracked" {
		f.Write(t, "local.txt", []byte("unpublished change\n"))
		f.CommitFile(t, "unpublished change")
		f.Git(t, f.Store(), "fetch", f.Work, "HEAD:"+ref)
	}
	f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/main")
	before := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)")
	notices.Store(0)
	gittest.ExpireRefresh(t, f.Store())
	var lost *gitport.ForgeHistoryLostError
	if err := c.Fetch(t.Context(), gittest.Repository); !errors.As(err, &lost) {
		t.Fatal("empty forge refresh accepted over cached history", err)
	}
	if after := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)"); after != before {
		t.Fatal("empty forge changed cached refs", before, after)
	}
	if notices.Load() != 0 {
		t.Fatal("refused empty refresh sent deletion notices")
	}
}

func TestEmptyForgeRefreshKeepsUnpublishedRefs(t *testing.T) {
	f := gittest.New(t)
	f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/main")
	var notices atomic.Int32
	cfg := reviewConfig(t, f)
	cfg.RefChanged = func(context.Context, string, []gitcache.RefUpdate) { notices.Add(1) }
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Store(), "fetch", f.Work, "HEAD:refs/heads/main", "HEAD:refs/oberth/keep/unpublished")
	before := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)")
	gittest.ExpireRefresh(t, f.Store())
	if err := c.Fetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal("local refs blocked empty-forge bootstrap", err)
	}
	if after := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)"); after != before {
		t.Fatal("empty refresh changed unpublished refs", before, after)
	}
	if notices.Load() != 0 {
		t.Fatal("empty refresh announced changes to unpublished refs")
	}
}
