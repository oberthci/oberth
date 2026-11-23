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
	var empty *gitport.EmptyRepositoryError
	if _, err := c.RemoteRef(t.Context(), gittest.Repository, "HEAD"); !errors.As(err, &empty) {
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

func checkEmptyForgeCachedHistory(t *testing.T, state string) {
	t.Helper()
	f := gittest.New(t)
	if state != "tracked" {
		f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/main")
	}
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
		f.Git(t, f.Store(), "fetch", f.Work, "HEAD:"+ref)
	}
	f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/main")
	before := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)")
	notices.Store(0)
	gittest.ExpireRefresh(t, f.Store())
	var empty *gitport.EmptyRepositoryError
	if err := c.Fetch(t.Context(), gittest.Repository); !errors.As(err, &empty) {
		t.Fatal("empty forge refresh accepted over cached history", err)
	}
	if after := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)"); after != before {
		t.Fatal("empty forge changed cached refs", before, after)
	}
	if notices.Load() != 0 {
		t.Fatal("refused empty refresh sent deletion notices")
	}
}
