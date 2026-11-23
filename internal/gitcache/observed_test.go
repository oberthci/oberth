package gitcache_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestObservedPublicationHeadSurvivesExpiredRefresh(t *testing.T) {
	f := gittest.New(t)
	c := f.Cache(t, nil)
	head, err := c.ObservedRef(t.Context(), gittest.Repository, "refs/heads/main")
	if err != nil || head.ObjectSHA != f.Commit {
		t.Fatal("tracked predecessor was not available", head, err)
	}
	gittest.ExpireRefresh(t, f.Store())
	again, err := c.ObservedRef(t.Context(), gittest.Repository, "refs/heads/main")
	if err != nil || again != head {
		t.Fatal("observed lease was lost with freshness metadata", again, err)
	}
}
