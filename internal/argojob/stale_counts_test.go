package argojob_test

import (
	"slices"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLockFactsDoNotAddDifferentStaleCounts(t *testing.T) {
	const lock = "runs/ConfigMap/limits/heavy"
	ended := wf.Workflow{ObjectMeta: meta.ObjectMeta{Namespace: "runs", Name: "ended"}, Spec: wf.WorkflowSpec{Shutdown: wf.ShutdownStrategyTerminate}, Status: wf.WorkflowStatus{Phase: wf.WorkflowFailed}}
	waiter := func(name string, holders []string) wf.Workflow {
		return wf.Workflow{ObjectMeta: meta.ObjectMeta{Namespace: "runs", Name: name}, Status: wf.WorkflowStatus{Phase: wf.WorkflowPending, Synchronization: &wf.SynchronizationStatus{Semaphore: &wf.SemaphoreStatus{Waiting: []wf.SemaphoreHolding{{Semaphore: lock, Holders: holders}}}}}}
	}
	for _, reverse := range []bool{false, true} {
		rows := []wf.Workflow{ended, waiter("two", []string{"runs/ended/one", "runs/ended/two"}), waiter("one", []string{"runs/ended/one"})}
		if reverse {
			slices.Reverse(rows)
		}
		facts, err := argojob.LockFacts(t.Context(), rows)
		if err != nil {
			t.Fatal(err)
		}
		stale, permits := 0, 0
		for _, fact := range facts {
			if fact.Role == argojob.LockStale {
				stale++
				permits += fact.Permits
			}
		}
		if stale != 1 || permits != 2 {
			t.Fatal("stale snapshots counted twice", stale, permits)
		}
	}
}
