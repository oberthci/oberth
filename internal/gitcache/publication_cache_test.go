package gitcache_test

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

// preparedPromotion returns a cache and a prepared promotion of a new commit
// onto main; nothing is published yet.
func preparedPromotion(t *testing.T, f *gittest.Fixture) (*gitcache.Cache, gitport.Promotion) {
	t.Helper()
	c := reviewNew(t, reviewConfig(t, f))
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Write(t, "promoted", []byte("promoted"))
	next := f.CommitFile(t, "promoted")
	f.Git(t, f.Store(), "fetch", f.Work, next)
	p, err := c.PreparePromotion(t.Context(), gitport.PromotionRequest{Repository: gittest.Repository,
		ActionReceipt: "promotion", Source: gitport.Revision{Repository: gittest.Repository, Commit: next},
		Target: gitport.Ref{Name: "refs/heads/main", ObjectSHA: f.Commit, CommitSHA: f.Commit}})
	if err != nil {
		t.Fatal(err)
	}
	return c, p
}

func stopForge(t *testing.T, f *gittest.Fixture) {
	t.Helper()
	if err := os.Rename(f.Bare, f.Bare+".offline"); err != nil {
		t.Fatal(err)
	}
	gittest.ExpireRefresh(t, f.Store())
}

func cacheRefs(t *testing.T, f *gittest.Fixture) string {
	t.Helper()
	return f.Git(t, f.Store(), "for-each-ref", "--format=%(refname) %(objectname)")
}

// A delivered promotion is the cache's main at once: a fetch right after it,
// even with the forge stopped, serves the promoted commit.
func TestPublishedPromotionIsServedWithTheForgeStopped(t *testing.T) {
	f := gittest.New(t)
	c, p := preparedPromotion(t, f)
	if _, err := c.Publish(t.Context(), promotionPublication(f, p)); err != nil {
		t.Fatal(err)
	}
	stopForge(t, f)
	var out, notice bytes.Buffer
	err := c.Upload(t.Context(), gittest.Repository, gitcache.ServiceIO{Input: io.NopCloser(strings.NewReader("0000")), Output: &out, Diagnostics: &notice})
	if err != nil || !strings.Contains(out.String(), p.Result.CommitSHA+" refs/heads/main") {
		t.Fatal("the cache does not serve the published promotion", err, out.String())
	}
	if !strings.HasPrefix(notice.String(), gitport.ForgeCacheNotice) {
		t.Fatal("the read did not try the stopped forge", notice.String())
	}
}

// A publication the forge never took moves nothing in the cache.
func TestFailedPublicationLeavesTheCacheAsItWas(t *testing.T) {
	for name, fail := range map[string]func(*testing.T, *gittest.Fixture){
		"forge stopped": stopForge,
		"forge main moved": func(t *testing.T, f *gittest.Fixture) {
			f.Write(t, "elsewhere", []byte("elsewhere"))
			f.CommitFile(t, "elsewhere")
			f.Git(t, f.Work, "push", "--force", f.Bare, "HEAD:refs/heads/main")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := gittest.New(t)
			c, p := preparedPromotion(t, f)
			fail(t, f)
			before := cacheRefs(t, f)
			if _, err := c.Publish(t.Context(), promotionPublication(f, p)); err == nil {
				t.Fatal("publication succeeded")
			}
			if after := cacheRefs(t, f); after != before {
				t.Fatal("a failed publication changed the cache", before, after)
			}
		})
	}
}
