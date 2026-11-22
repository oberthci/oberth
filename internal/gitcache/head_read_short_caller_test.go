package gitcache_test

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// A caller whose own context ends before its head wait (a cancelled request,
// an admission at its deadline) does not mark the forge's head read as
// unanswered: the next caller waits for the read, and the forge's rewritten
// head refuses a commit the cache still holds, instead of the cache deciding
// at once.
func TestShortCallerDoesNotMarkTheHeadReadUnanswered(t *testing.T) {
	x := newPinFixture(t)
	x.start(t)
	x.forge.arm(t, "none")
	f := x.pipeline
	removed := f.Commit
	f.Write(t, "rewritten", []byte("history without the removed commit"))
	f.Git(t, f.Work, "checkout", "-q", "--orphan", "rewritten")
	rewritten := f.CommitFile(t, "rewritten root")
	f.Git(t, f.Work, "push", "-q", "--force", f.Bare, rewritten+":refs/heads/main")
	x.forge.arm(t, "pipeline")
	x.forge.holdOnly(t, "refs/heads/main^{}")
	gitcache.SetHeadWait(x.c, 2*time.Second)
	at := sourcecontract.Revision{Repository: pinnedPipeline, Commit: removed}
	short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, _, _ = x.c.Pin(short, at)
	answer := time.AfterFunc(300*time.Millisecond, x.forge.releaseOne)
	defer answer.Stop()
	if _, reachable, err := x.c.Pin(t.Context(), at); reachable {
		t.Fatal("a commit the forge's answered head excludes was decided from the cache after a short caller left", err)
	}
}
