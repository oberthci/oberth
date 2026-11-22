package gitcache_test

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// A forge that answers the head read decides, also when the transfer of its
// head does not finish within the head wait: a commit the forge's rewritten
// default branch no longer holds is refused (the copy is still updating),
// never admitted from the older head the cache holds.
func TestAnsweredHeadDecidesWhileItsTransferRuns(t *testing.T) {
	x := newFetchFixture(t, transferring)
	f := x.f
	f.Write(t, "removed", []byte("a commit the forge drops"))
	removed := f.CommitFile(t, "removed upstream later")
	f.Git(t, f.Work, "push", f.Bare, "main")
	c := x.start(t)
	f.Git(t, f.Work, "reset", "-q", "--hard", f.Commit)
	f.Write(t, "rewritten", []byte("the forge's new history"))
	f.CommitFile(t, "rewritten head")
	f.Git(t, f.Work, "push", "-q", "--force", f.Bare, "main")
	x.mu.Lock()
	x.now = x.now.Add(gitcache.RefreshInterval)
	x.mu.Unlock()
	gitcache.SetHeadWait(c, 500*time.Millisecond)
	head, reachable, err := c.DefaultAncestor(t.Context(), sourcecontract.Revision{Repository: gittest.Repository, Commit: removed})
	if reachable || err == nil {
		t.Fatal("a commit the answered forge head excludes was admitted from the cache", head, err)
	}
	checkStillUpdating(t, err)
	x.slow.release(t)
	gitcache.WaitBackground(c)
}

// A forge that answers that its default branch is gone decides as well: when
// the second listing, which tells an empty forge from a deleted default
// branch, does not finish within the head wait, the pin is not reachable and
// is never decided from the head the cache holds.
func TestAnsweredAbsenceDecidesWhileTheListingRuns(t *testing.T) {
	x := newPinFixture(t)
	x.start(t)
	f := x.pipeline
	f.Git(t, f.Work, "push", "-q", f.Bare, "main:refs/heads/other")
	f.Git(t, f.Bare, "update-ref", "-d", "refs/heads/main")
	x.forge.arm(t, "pipeline")
	x.forge.holdOnly(t, "refs/heads/*")
	gitcache.SetHeadWait(x.c, 300*time.Millisecond)
	_, reachable, err := x.c.Pin(t.Context(), sourcecontract.Revision{Repository: pinnedPipeline, Commit: f.Commit})
	if reachable || err != nil {
		t.Fatal("a pin of a deleted default branch was decided from the cache", reachable, err)
	}
}
