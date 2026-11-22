package gitcache_test

import (
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// A forge that answers its head reads more slowly than the head wait, with
// a refresh older than the trust window: the first pin is refused as a forge
// that did not answer in time; its read finishes after the pin left and
// fetches the head object; the retry, whose own read is slow again, decides
// from that answer as from one the forge just gave. An answer of another
// forge identity is never used.
func TestKeptHeadAnswerDecidesTheRetry(t *testing.T) {
	x := newPinFixture(t)
	x.start(t)
	f := x.pipeline
	f.Write(t, "next", []byte("a head the cache lacks"))
	next := f.CommitFile(t, "next head")
	f.Git(t, f.Work, "push", "-q", f.Bare, "main")
	x.forge.arm(t, "pipeline", "moved")
	x.forge.holdOnly(t, "refs/heads/main^{}")
	x.mu.Lock()
	x.now = x.now.Add(gitcache.CacheTrustWindow)
	x.mu.Unlock()
	gitcache.SetHeadWait(x.c, 300*time.Millisecond)
	at := sourcecontract.Revision{Repository: pinnedPipeline, Commit: next}
	checkSilentRefusal(t, x, at)
	x.forge.releaseOne()
	gitcache.WaitBackground(x.c)
	if _, reachable, err := x.c.Pin(t.Context(), at); err != nil || !reachable {
		t.Fatal("the retry was not decided from the forge's kept answer", reachable, err)
	}
	x.move(pinnedPipeline, x.moved)
	checkSilentRefusal(t, x, at)
}

func checkSilentRefusal(t *testing.T, x *pinFixture, at sourcecontract.Revision) {
	t.Helper()
	_, reachable, err := x.c.Pin(t.Context(), at)
	var refusal *publicerr.Error
	if reachable || !errors.As(err, &refusal) || refusal.Reason() != gitcache.ReasonForgeSilent {
		t.Fatal("a pin without a forge answer was not refused as a forge that did not answer", reachable, err)
	}
}
