package sync

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/argoproj/argo-workflows/v4/util/logging"
)

// A sibling may acquire ahead of its workflow's first queued node. Argo #16567
// removed that first node instead of the sibling. After the sibling's release,
// a competing workflow could take the slot and never wake the dropped node.
func TestOberthSemaphoreWaiterWakes(t *testing.T) {
	ctx := logging.TestContext(t.Context())
	var notified []string
	sem, err := newInternalSemaphore(ctx, "ci/ConfigMap/limits/build", func(workflow string) {
		notified = append(notified, workflow)
	}, func(context.Context, string) (int, error) { return 1, nil }, 0)
	if err != nil {
		t.Fatal(err)
	}
	first, sibling, competitor := "ci/run/first", "ci/run/sibling", "ci/other/step"
	for i, node := range []string{first, sibling, competitor} {
		if err := sem.addToQueue(ctx, node, 0, time.Unix(int64(i), 0)); err != nil {
			t.Fatal(err)
		}
	}
	acquire := func(node string) bool {
		t.Helper()
		ok, _, err := sem.tryAcquire(ctx, node, nil)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	release := func(node string) {
		t.Helper()
		// This is the order used by sync.Manager.Release.
		sem.release(ctx, node)
		if err := sem.removeFromQueue(ctx, node); err != nil {
			t.Fatal(err)
		}
	}
	if !acquire(sibling) {
		t.Fatal("sibling could not acquire ahead of its workflow's first node")
	}
	if acquire(first) || acquire(competitor) {
		t.Fatal("one-slot semaphore admitted another holder")
	}
	release(sibling)
	if !slices.Contains(notified, "ci/run") {
		t.Fatal("release did not wake the waiting workflow")
	}
	notified = nil
	if !acquire(first) {
		if !acquire(competitor) {
			t.Fatal("competing workflow could not take the released slot")
		}
		release(competitor)
		pending, _ := sem.getCurrentPending(ctx)
		t.Fatalf("same-workflow waiter lost: free slot, pending=%v, wakeups=%v; missed wakeup=%v",
			pending, notified, !slices.Contains(notified, "ci/run"))
	}
	release(first)
	if !slices.Contains(notified, "ci/other") || !acquire(competitor) {
		t.Fatal("competitor did not wake and acquire after the first node")
	}
	release(competitor)
	pending, err := sem.getCurrentPending(ctx)
	if err != nil || len(pending) != 0 || len(sem.lockHolder) != 0 {
		t.Fatal("finished nodes left queued entries or holders", pending, err)
	}
}
