package gitcache_test

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func (x *pinFixture) move(repo string, f *gittest.Fixture) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.remotes[repo] = f
}

// A cache refreshed from a repository's earlier forge address never decides
// for its new address: after the registration moved, a silent new forge
// refuses the pin although the cache's head reaches the commit.
func TestRefreshFromAnotherForgeNeverTrustsTheCache(t *testing.T) {
	x := newPinFixture(t)
	x.start(t)
	gitcache.SetHeadWait(x.c, 300*time.Millisecond)
	x.move(pinnedPipeline, x.moved)
	_, reachable, err := x.c.Pin(t.Context(), sourcecontract.Revision{Repository: pinnedPipeline, Commit: x.pipeline.Commit})
	var refusal *publicerr.Error
	if reachable || !errors.As(err, &refusal) || refusal.Reason() != gitcache.ReasonForgeSilent {
		t.Fatal("a cache refreshed from another forge decided the pin", reachable, err)
	}
}

// A head read still running against a repository's earlier forge never
// answers a caller whose registration names another forge: that caller
// reads the new forge's head.
func TestHeadJobOfAnotherForgeNeverAnswers(t *testing.T) {
	x := newPinFixture(t)
	m := x.moved
	m.Write(t, "moved", []byte("the new forge's own head"))
	movedHead := m.CommitFile(t, "moved head")
	m.Git(t, m.Work, "push", "-q", m.Bare, "main")
	x.start(t)
	x.forge.arm(t, "pipeline")
	gitcache.SetHeadWait(x.c, 300*time.Millisecond)
	at := sourcecontract.Revision{Repository: pinnedPipeline, Commit: x.pipeline.Commit}
	if _, reachable, err := x.c.Pin(t.Context(), at); err != nil || !reachable {
		t.Fatal("the fresh cache did not decide while the forge was silent", reachable, err)
	}
	x.move(pinnedPipeline, x.moved)
	head, _, err := x.c.DefaultAncestor(t.Context(), at)
	if err != nil || head.CommitSHA != movedHead {
		t.Fatal("the new forge's head did not decide", head, err)
	}
}

// A head object fetch that outlives a move of the repository's registration
// imports nothing from the earlier forge, as a refresh does.
func TestHeadObjectOfAMovedUpstreamImportsNothing(t *testing.T) {
	x := newPinFixture(t)
	x.start(t)
	x.forge.arm(t, "pipeline")
	x.forge.holdOnly(t, "fetch")
	gitcache.SetHeadWait(x.c, 300*time.Millisecond)
	f := x.pipeline
	f.Write(t, "next", []byte("a head the cache lacks"))
	next := f.CommitFile(t, "next head")
	f.Git(t, f.Work, "push", "-q", f.Bare, "main")
	if _, reachable, err := x.c.DefaultAncestor(t.Context(), sourcecontract.Revision{Repository: pinnedPipeline, Commit: next}); err == nil || reachable {
		t.Fatal("a head whose transfer runs decided", reachable, err)
	}
	x.move(pinnedPipeline, x.moved)
	x.forge.release()
	gitcache.WaitBackground(x.c)
	store := filepath.Join(x.f.Root, "cache", "origin", "team", "pipeline.git")
	if err := exec.Command(x.f.Binary, "-C", store, "cat-file", "-e", next).Run(); err == nil {
		t.Fatal("the earlier forge's head object was imported after the move")
	}
}
