package gitcache_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestRemoteHeadSupportsRepositoryProbe(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	cache := f.Cache(t, nil)
	f.Git(t, f.Bare, "symbolic-ref", "HEAD", "refs/heads/main")
	ref, err := cache.RemoteRef(t.Context(), gittest.Repository, "HEAD")
	if err != nil || ref.ObjectSHA != f.Commit || ref.CommitSHA != f.Commit {
		t.Fatal("remote HEAD probe failed", ref, err)
	}
	f.Git(t, f.Bare, "symbolic-ref", "HEAD", "refs/heads/missing-head")
	if _, err = cache.RemoteRef(t.Context(), gittest.Repository, "HEAD"); err == nil {
		t.Fatal("missing remote HEAD reported readable")
	}
}
