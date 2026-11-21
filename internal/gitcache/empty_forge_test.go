package gitcache_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

func TestEmptyForgeRefusesCloneAndAcceptsFirstCommit(t *testing.T) {
	f := gittest.New(t)
	f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/main")
	c := reviewNew(t, reviewConfig(t, f))
	err := c.Init(t.Context(), gittest.Repository)
	var empty *gitport.EmptyRepositoryError
	if !errors.As(err, &empty) || empty.Remedy() != gitport.EmptyForgeRemedy {
		t.Fatal("missing empty forge remedy", err)
	}
	if _, err = os.Stat(filepath.Join(f.Store(), "HEAD")); !os.IsNotExist(err) {
		t.Fatal("empty clone became canonical", err)
	}
	f.Git(t, f.Bare, "update-ref", "refs/heads/main", f.Commit)
	if err = c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal("first commit did not unblock clone", err)
	}
	f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/main")
	if _, err = c.RemoteRef(t.Context(), gittest.Repository, "HEAD"); !errors.As(err, &empty) {
		t.Fatal("existing cache hid empty forge", err)
	}
}
