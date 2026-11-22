package gitcache_test

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// A producer whose head reads do not answer in time and whose cache is not
// trusted (its last refresh is older than the trust window, or predates the
// record naming the forge) is refused, and that refusal starts the
// producer's refresh: once it lands, the next pin is decided from the
// refreshed cache. Admission never waits for that refresh.
func TestUntrustedCacheIsRefreshedWhileTheForgeHeadReadIsSlow(t *testing.T) {
	x := newPinFixture(t)
	x.start(t)
	x.forge.arm(t, "pipeline")
	x.forge.holdOnly(t, "refs/heads/main^{}")
	gitcache.DeltaSetNativeIdle(x.c, 300*time.Millisecond)
	gitcache.SetHeadWait(x.c, 100*time.Millisecond)
	x.mu.Lock()
	x.now = x.now.Add(gitcache.CacheTrustWindow)
	x.mu.Unlock()
	at := sourcecontract.Revision{Repository: pinnedPipeline, Commit: x.pipeline.Commit}
	checkSilentRefusal(t, x, at)
	gitcache.WaitBackground(x.c)
	if _, reachable, err := x.c.Pin(t.Context(), at); err != nil || !reachable {
		t.Fatal("the producer's cache was not refreshed after a refusal for an untrusted cache", reachable, err)
	}
}
