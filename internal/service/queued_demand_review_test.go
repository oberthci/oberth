package service

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type queueDemandReviewReader struct {
	ResourceDeclarationReader
	calls atomic.Int32
	read  func(context.Context) (*argoworkflow.PodDemand, error)
}

func (r *queueDemandReviewReader) RunPodDemand(ctx context.Context, _ WorkflowRequest) (*argoworkflow.PodDemand, error) {
	r.calls.Add(1)
	return r.read(ctx)
}

func queueDemandListFixture(t *testing.T, count int, reader *queueDemandReviewReader) *API {
	t.Helper()
	f := newControlFixture(t)
	for i := range count {
		_, err := f.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: fmt.Sprintf("feature/queued-%d", i), SHA: fmt.Sprintf("%040x", i+1), Actor: "reader", Trigger: "push"})
		if err != nil {
			t.Fatal(err)
		}
	}
	control := f.api(t)
	control.resourceDeclarations = reader
	return control
}

func TestQueuedListsKeepSuccessfulDemandAcrossPolls(t *testing.T) {
	for _, view := range []string{""} {
		t.Run(view, func(t *testing.T) {
			const count = 20
			reader := &queueDemandReviewReader{read: func(context.Context) (*argoworkflow.PodDemand, error) {
				return &argoworkflow.PodDemand{CPUMilli: 9900, MemoryBytes: 16 << 30, Pods: 2}, nil
			}}
			control := queueDemandListFixture(t, count, reader)
			clock := clocktesting.NewFakeClock(time.Now())
			control.waitClock = clock
			for poll := range 3 {
				clock.Step(time.Hour) // Success must not expire after 30 seconds.
				value := pollDiagnosticList(t, control, api.Actor{Identity: "reader"}, api.RunFilter{View: view, Limit: 100}, func(_ []model.Run) bool { return reader.calls.Load() == count })

				rows := len(value)
				if rows != count || reader.calls.Load() != count {
					t.Fatalf("poll %d rows=%d source reads=%d, want %d then zero more", poll, rows, reader.calls.Load(), count)
				}
			}
		})
	}
}

func TestQueuedListsDoNotWaitForDemandAndCacheFailures(t *testing.T) {
	reader := &queueDemandReviewReader{read: func(ctx context.Context) (*argoworkflow.PodDemand, error) { <-ctx.Done(); return nil, ctx.Err() }}
	const count = 8
	control := queueDemandListFixture(t, count, reader)
	for range 2 {
		started := time.Now()
		if _, err := control.Runs(t.Context(), api.Actor{Identity: "reader"}, api.RunFilter{Limit: 100}); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
			t.Fatalf("list blocked on demand: %s", elapsed)
		}
	}
	drainListDiagnostics(t, control)
	if got := reader.calls.Load(); got != count {
		t.Fatalf("some rows never refreshed: %d", got)
	}
	// Restrict the next page to the first row, whose failed result is cached.
	value, err := control.Runs(t.Context(), api.Actor{Identity: "reader"}, api.RunFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	run := value.([]model.Run)[0]
	drainListDiagnostics(t, control)
	calls := reader.calls.Load()
	if _, err := control.Runs(t.Context(), api.Actor{Identity: "reader"}, api.RunFilter{Ref: run.Ref}); err != nil {
		t.Fatal(err)
	}
	drainListDiagnostics(t, control)
	if reader.calls.Load() != calls {
		t.Fatal("failed demand refreshed before expiry")
	}
}

func TestQueueDemandLRUEvictionAndNegativeExpiry(t *testing.T) {
	cache := &queueDemandCache{}
	now := time.Now()
	clock := func() time.Time { return now }
	key := func(i int) queueDemandKey { return queueDemandKey{runID: fmt.Sprint(i), sha: "sha"} }
	calls := 0
	load := func() *argoworkflow.PodRequests { calls++; return &argoworkflow.PodRequests{CPUMilli: 1000} }
	for i := range queueDemandLimit {
		cache.read(t.Context(), key(i), clock, load)
	}
	cache.read(t.Context(), key(0), clock, load) // Protect most recently used.
	cache.read(t.Context(), key(queueDemandLimit), clock, load)
	if cache.recent.Len() != queueDemandLimit {
		t.Fatal("unbounded cache")
	}
	now = now.Add(24 * time.Hour)
	cache.read(t.Context(), key(0), clock, load)
	if calls != queueDemandLimit+1 {
		t.Fatal("successful result expired or MRU was evicted")
	}
	cache.read(t.Context(), key(1), clock, load)
	if calls != queueDemandLimit+2 {
		t.Fatal("least recently used result was not evicted")
	}
	failed := 0
	negative := func() *argoworkflow.PodRequests { failed++; return nil }
	cache.read(t.Context(), key(-1), clock, negative)
	cache.read(t.Context(), key(-1), clock, negative)
	if failed != 1 {
		t.Fatal("failure was not cached")
	}
	now = now.Add(queueDemandNegativeTTL + time.Second)
	cache.read(t.Context(), key(-1), clock, negative)
	if failed != 2 {
		t.Fatal("failed lookup never retried")
	}
	// A changed SHA must not reuse the prior graph.
	cache.read(t.Context(), queueDemandKey{runID: "0", sha: "new-sha"}, clock, load)
	if calls != queueDemandLimit+3 {
		t.Fatal("changed SHA reused demand")
	}
}

func TestQueueDemandCoalescesConcurrentMisses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := &queueDemandCache{}
		var calls atomic.Int32
		var workers sync.WaitGroup
		for range 10 {
			workers.Go(func() {
				got := cache.read(t.Context(), queueDemandKey{runID: "same", sha: "sha"}, time.Now, func() *argoworkflow.PodRequests {
					calls.Add(1)
					time.Sleep(time.Second)
					return &argoworkflow.PodRequests{CPUMilli: 1000}
				})
				if got == nil || got.CPUMilli != 1000 {
					t.Error("missing coalesced result")
				}
			})
		}
		workers.Wait()
		if calls.Load() != 1 {
			t.Fatal("concurrent misses repeated source reads", calls.Load())
		}
	})
}
