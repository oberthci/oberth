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
