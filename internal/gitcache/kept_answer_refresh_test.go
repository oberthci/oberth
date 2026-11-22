package gitcache_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// answeredThenRefreshed lets the pipeline forge answer a pin of its head
// (the answer is kept), then moves the forge with move, refreshes the cache
// from it, and holds every later head read of that forge past the head wait.
func answeredThenRefreshed(t *testing.T, move func(*pinFixture)) *pinFixture {
	t.Helper()
	x := newPinFixture(t)
	x.start(t)
	x.forge.arm(t, "none")
	gitcache.SetHeadWait(x.c, 300*time.Millisecond)
	if _, reachable, err := x.c.Pin(t.Context(), sourcecontract.Revision{Repository: pinnedPipeline, Commit: x.pipeline.Commit}); err != nil || !reachable {
		t.Fatal("the answering forge did not decide", reachable, err)
	}
	move(x)
	x.mu.Lock()
	x.now = x.now.Add(gitcache.RefreshInterval)
	x.mu.Unlock()
	if err := x.c.Fetch(t.Context(), pinnedPipeline); err != nil {
		t.Fatal(err)
	}
	x.forge.arm(t, "pipeline")
	x.forge.holdOnly(t, "refs/heads/main^{}")
	return x
}

// A kept answer never overrides what the cache learned after it: once a
// refresh brought the forge's rewritten head, a commit the rewrite removed is
// refused while the forge's read is slow.
func TestKeptAnswerOlderThanTheRefreshNeverAdmitsARemovedCommit(t *testing.T) {
	var removed string
	x := answeredThenRefreshed(t, func(x *pinFixture) {
		f := x.pipeline
		removed = f.Commit
		f.Write(t, "rewritten", []byte("history without the removed commit"))
		f.Git(t, f.Work, "checkout", "-q", "--orphan", "rewritten")
		rewritten := f.CommitFile(t, "rewritten root")
		f.Git(t, f.Work, "push", "-q", "--force", f.Bare, rewritten+":refs/heads/main")
	})
	_, reachable, err := x.c.Pin(t.Context(), sourcecontract.Revision{Repository: pinnedPipeline, Commit: removed})
	if reachable || err == nil {
		t.Fatal("a commit the refreshed cache knows is removed was admitted from an older kept answer", reachable, err)
	}
}

// Nor does it refuse what the cache learned after it: once a refresh brought
// the forge's newer head, a pin of that head is admitted while the forge's
// read is slow.
func TestKeptAnswerOlderThanTheRefreshNeverRefusesALandedCommit(t *testing.T) {
	var landed string
	x := answeredThenRefreshed(t, func(x *pinFixture) {
		f := x.pipeline
		f.Write(t, "landed", []byte("a commit landed after the answer"))
		landed = f.CommitFile(t, "landed")
		f.Git(t, f.Work, "push", "-q", f.Bare, "main")
	})
	if _, reachable, err := x.c.Pin(t.Context(), sourcecontract.Revision{Repository: pinnedPipeline, Commit: landed}); err != nil || !reachable {
		t.Fatal("a commit the refreshed cache holds was refused from an older kept answer", reachable, err)
	}
}

// Oberth's own delivery moves the tracking ref without a refresh record: it
// forgets the kept answers too, so a pin of the delivered commit is decided
// from the cache that holds it while the forge's read is slow.
func TestDeliveryForgetsTheKeptAnswer(t *testing.T) {
	x := newPinFixture(t)
	x.start(t)
	x.forge.arm(t, "none")
	gitcache.SetHeadWait(x.c, 300*time.Millisecond)
	f := x.pipeline
	previous := f.Commit
	if _, reachable, err := x.c.Pin(t.Context(), sourcecontract.Revision{Repository: pinnedPipeline, Commit: previous}); err != nil || !reachable {
		t.Fatal("the answering forge did not decide", reachable, err)
	}
	f.Write(t, "delivered", []byte("a commit Oberth delivers"))
	delivered := f.CommitFile(t, "delivered")
	store := filepath.Join(x.f.Root, "cache", "origin", "team", "pipeline.git")
	f.Git(t, store, "fetch", "-q", f.Work, delivered)
	f.Git(t, store, "update-ref", "refs/heads/main", delivered)
	publication := model.Publication{ID: "deliver", Repo: pinnedPipeline, Ref: "refs/heads/main", ObjectSHA: delivered, CommitSHA: &delivered, PreviousSHA: &previous, Actor: "actor"}
	if _, err := x.c.Publish(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	x.forge.arm(t, "pipeline")
	x.forge.holdOnly(t, "refs/heads/main^{}")
	if _, reachable, err := x.c.Pin(t.Context(), sourcecontract.Revision{Repository: pinnedPipeline, Commit: delivered}); err != nil || !reachable {
		t.Fatal("a delivered commit was refused from an older kept answer", reachable, err)
	}
}
