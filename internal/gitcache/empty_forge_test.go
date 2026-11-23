package gitcache_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

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
