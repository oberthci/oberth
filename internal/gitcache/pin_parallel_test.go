package gitcache_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

type pinResult struct {
	id        int64
	reachable bool
	err       error
}

func pinsTogether(ctx context.Context, source interface {
	Pin(context.Context, sourcecontract.Revision) (int64, bool, error)
}, pins []sourcecontract.Revision) []pinResult {
	out := make([]pinResult, len(pins))
	var wg sync.WaitGroup
	for i, at := range pins {
		wg.Go(func() { out[i].id, out[i].reachable, out[i].err = source.Pin(ctx, at) })
	}
	wg.Wait()
	return out
}

// One cache serves all producers. Both head waits must fit before a second
// serial wait could finish; duplicate revisions share the same forge job.
func TestConcurrentPinsShareOneCacheAndKeepTrustRules(t *testing.T) {
	x := newPinFixture(t)
	f := x.pipeline
	f.Git(t, f.Work, "checkout", "-q", "-b", "side")
	f.Write(t, "side", []byte("off the default branch"))
	side := f.CommitFile(t, "side commit")
	f.Git(t, f.Work, "push", "-q", f.Bare, "side")
	x.start(t)
	gitcache.SetHeadWait(x.c, time.Second)
	pins := []sourcecontract.Revision{
		{Repository: pinnedPipeline, Commit: f.Commit},
		{Repository: pinnedPipeline, Commit: side},
		{Repository: pinnedWire, Commit: x.wire.Commit},
		{Repository: pinnedWire, Commit: x.wire.Commit},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 1800*time.Millisecond)
	defer cancel()
	got := pinsTogether(ctx, x.c, pins)
	for _, i := range []int{0, 2, 3} {
		if got[i].err != nil || !got[i].reachable || got[i].id != int64(len(pins[i].Repository)) {
			t.Fatal("fresh cache did not decide concurrent pins", got)
		}
	}
	requireSilentPin(t, got[1])
	if ctx.Err() != nil || x.forge.asked(t, "pipeline") != 1 || x.forge.asked(t, "wire") != 1 {
		t.Fatal("head reads were serialized or repeated", ctx.Err())
	}
	x.mu.Lock()
	x.now = x.now.Add(gitcache.CacheTrustWindow)
	x.mu.Unlock()
	for _, result := range pinsTogether(t.Context(), x.c, pins) {
		requireSilentPin(t, result)
	}
}

func requireSilentPin(t *testing.T, result pinResult) {
	t.Helper()
	var refusal *publicerr.Error
	if result.reachable || !errors.As(result.err, &refusal) || refusal.Reason() != gitcache.ReasonForgeSilent {
		t.Fatal("unreachable or stale pin escaped the silent-forge refusal", result)
	}
}

func TestAnsweredHeadDecidesBesideConcurrentSilentPin(t *testing.T) {
	x := newPinFixture(t)
	f := x.pipeline
	f.Git(t, f.Work, "checkout", "-q", "--orphan", "other")
	f.Write(t, "other", []byte("disconnected history"))
	other := f.CommitFile(t, "other history")
	f.Git(t, f.Work, "push", "-q", f.Bare, "other")
	x.start(t)
	f.Git(t, f.Bare, "update-ref", "refs/heads/main", other)
	x.forge.arm(t, "wire")
	gitcache.SetHeadWait(x.c, 300*time.Millisecond)
	pins := []sourcecontract.Revision{
		{Repository: pinnedPipeline, Commit: f.Commit},
		{Repository: pinnedPipeline, Commit: other},
		{Repository: pinnedWire, Commit: x.wire.Commit},
	}
	got := pinsTogether(t.Context(), x.c, pins)
	if got[0].reachable || got[0].err != nil {
		t.Fatal("fresh cache overrode the answered head", got[0])
	}
	for _, i := range []int{1, 2} {
		if !got[i].reachable || got[i].err != nil {
			t.Fatal("answered head or silent producer's cache did not decide", got[i])
		}
	}
}
