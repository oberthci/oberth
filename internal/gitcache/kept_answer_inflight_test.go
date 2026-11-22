package gitcache_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// inFlightRead starts a pin whose head read of the pipeline forge asks the
// forge at once and hands its answer over only when released, so the read is
// still in flight when the test moves the cache's tracking refs.
func inFlightRead(t *testing.T) *pinFixture {
	t.Helper()
	x := newPinFixture(t)
	x.start(t)
	gitcache.SetHeadWait(x.c, 300*time.Millisecond)
	x.forge.arm(t, "pipeline")
	x.forge.holdOnly(t, "refs/heads/main^{}")
	x.forge.deliverLate(t)
	_, _, _ = x.c.Pin(t.Context(), sourcecontract.Revision{Repository: pinnedPipeline, Commit: x.pipeline.Commit})
	return x
}

// lateAnswer hands the in-flight read's answer over after the tracking refs
// moved, and waits for the read to end.
func (x *pinFixture) lateAnswer(t *testing.T) {
	t.Helper()
	x.forge.releaseOne()
	gitcache.WaitBackground(x.c)
	x.forge.arm(t, "pipeline")
}

// A head read that asked the forge before a refresh and answers after it
// never keeps its older answer: once the refresh brought the forge's
// rewritten head, a commit the rewrite removed is refused while the forge's
// next read is slow.
func TestReadInFlightAcrossARefreshKeepsNoAnswer(t *testing.T) {
	x := inFlightRead(t)
	f := x.pipeline
	removed := f.Commit
	f.Write(t, "rewritten", []byte("history without the removed commit"))
	f.Git(t, f.Work, "checkout", "-q", "--orphan", "rewritten")
	rewritten := f.CommitFile(t, "rewritten root")
	f.Git(t, f.Work, "push", "-q", "--force", f.Bare, rewritten+":refs/heads/main")
	x.forge.arm(t, "none")
	x.mu.Lock()
	x.now = x.now.Add(gitcache.RefreshInterval)
	x.mu.Unlock()
	if err := x.c.Fetch(t.Context(), pinnedPipeline); err != nil {
		t.Fatal(err)
	}
	x.lateAnswer(t)
	if _, reachable, err := x.c.Pin(t.Context(), sourcecontract.Revision{Repository: pinnedPipeline, Commit: removed}); reachable || err == nil {
		t.Fatal("a commit the refreshed cache knows is removed was admitted from an answer older than the refresh", reachable, err)
	}
}

// Nor after a delivery: once Oberth delivered a new head, a pin of it is
// admitted while the forge's next read is slow, not refused from the
// answer the earlier read handed over late.
func TestReadInFlightAcrossADeliveryKeepsNoAnswer(t *testing.T) {
	x := inFlightRead(t)
	f := x.pipeline
	previous := f.Commit
	f.Write(t, "delivered", []byte("a commit Oberth delivers"))
	delivered := f.CommitFile(t, "delivered")
	store := filepath.Join(x.f.Root, "cache", "origin", "team", "pipeline.git")
	f.Git(t, store, "fetch", "-q", f.Work, delivered)
	f.Git(t, store, "update-ref", "refs/heads/main", delivered)
	x.forge.arm(t, "none")
	publication := model.Publication{ID: "deliver", Repo: pinnedPipeline, Ref: "refs/heads/main", ObjectSHA: delivered, CommitSHA: &delivered, PreviousSHA: &previous, Actor: "actor"}
	if _, err := x.c.Publish(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	x.lateAnswer(t)
	if _, reachable, err := x.c.Pin(t.Context(), sourcecontract.Revision{Repository: pinnedPipeline, Commit: delivered}); err != nil || !reachable {
		t.Fatal("a delivered commit was refused from an answer handed over after the delivery", reachable, err)
	}
}

// The generation is per repository: a refresh of another repository while a
// head read is in flight leaves that read's answer in place, and it decides
// the retry whose own read is slow.
func TestReadInFlightAcrossAnotherRepositorysRefreshKeepsItsAnswer(t *testing.T) {
	x := newPinFixture(t)
	x.start(t)
	gitcache.SetHeadWait(x.c, 300*time.Millisecond)
	x.mu.Lock()
	x.now = x.now.Add(gitcache.CacheTrustWindow)
	x.mu.Unlock()
	x.forge.arm(t, "pipeline")
	x.forge.holdOnly(t, "refs/heads/main^{}")
	x.forge.deliverLate(t)
	at := sourcecontract.Revision{Repository: pinnedPipeline, Commit: x.pipeline.Commit}
	checkSilentRefusal(t, x, at)
	if err := x.c.Fetch(t.Context(), pinnedWire); err != nil {
		t.Fatal(err)
	}
	x.lateAnswer(t)
	if _, reachable, err := x.c.Pin(t.Context(), at); err != nil || !reachable {
		t.Fatal("another repository's refresh dropped the in-flight read's answer", reachable, err)
	}
}
