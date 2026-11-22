package gitcache_test

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// A head read whose forge session stalls (nothing arrives) ends at the idle
// limit like a transfer does, so the next admission read asks the forge again
// instead of joining a read that holds its job for the whole local budget.
func TestStalledHeadReadEndsAtTheIdleLimit(t *testing.T) {
	x := newPinFixture(t)
	x.start(t)
	x.forge.arm(t, "pipeline")
	gitcache.DeltaSetNativeIdle(x.c, 300*time.Millisecond)
	gitcache.SetHeadWait(x.c, 100*time.Millisecond)
	at := sourcecontract.Revision{Repository: pinnedPipeline, Commit: x.pipeline.Commit}
	_, _, _ = x.c.Pin(t.Context(), at)
	ended := make(chan struct{})
	go func() { gitcache.WaitBackground(x.c); close(ended) }()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled head read was not ended at the idle limit")
	}
	_, _, _ = x.c.Pin(t.Context(), at)
	if got := x.forge.asked(t, "pipeline"); got != 2 {
		t.Fatal("the next read did not ask the forge again", got)
	}
}
