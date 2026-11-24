package argojob_test

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestLockFactsCountTwoStepsOfOneRun(t *testing.T) {
	const lock = "runs/ConfigMap/limits/heavy"
	rows := []wf.Workflow{{ObjectMeta: meta.ObjectMeta{Namespace: "runs", Name: "build"}, Status: wf.WorkflowStatus{
		Phase:           wf.WorkflowRunning,
		Nodes:           wf.Nodes{"one": {DisplayName: "compile"}, "two": {DisplayName: "test"}},
		Synchronization: &wf.SynchronizationStatus{Semaphore: &wf.SemaphoreStatus{Holding: []wf.SemaphoreHolding{{Semaphore: lock, Holders: []string{"runs/build/one", "runs/build/two"}}}}},
	}}}
	facts, err := argojob.LockFacts(t.Context(), rows)
	if err != nil || len(facts) != 1 || facts[0].Permits != 2 || facts[0].Steps != "compile, test" || facts[0].Role != argojob.LockHolder {
		t.Fatal("two permits collapsed to one run", facts, err)
	}
	rows[0].Status.Phase = wf.WorkflowSucceeded
	facts, err = argojob.LockFacts(t.Context(), rows)
	if err != nil || len(facts) != 0 {
		t.Fatal("ended run still holds permits", facts, err)
	}
}

func TestLockFactsCountRepositoryMutex(t *testing.T) {
	rows := []wf.Workflow{{ObjectMeta: meta.ObjectMeta{Namespace: "runs", Name: "build"}, Status: wf.WorkflowStatus{
		Phase:           wf.WorkflowRunning,
		Synchronization: &wf.SynchronizationStatus{Mutex: &wf.MutexStatus{Holding: []wf.MutexHolding{{Mutex: "runs/Mutex/repo", Holder: "runs/build"}}}},
	}}}
	facts, err := argojob.LockFacts(t.Context(), rows)
	if err != nil || len(facts) != 1 || facts[0].Permits != 1 || facts[0].Steps != "workflow" {
		t.Fatal(facts, err)
	}
}

func TestLockFactsCountStalePermits(t *testing.T) {
	const lock = "runs/ConfigMap/limits/heavy"
	rows := []wf.Workflow{
		{ObjectMeta: meta.ObjectMeta{Namespace: "runs", Name: "ended"}, Spec: wf.WorkflowSpec{Shutdown: wf.ShutdownStrategyTerminate}, Status: wf.WorkflowStatus{Phase: wf.WorkflowFailed}},
		{ObjectMeta: meta.ObjectMeta{Namespace: "runs", Name: "waiting"}, Status: wf.WorkflowStatus{Phase: wf.WorkflowPending, Synchronization: &wf.SynchronizationStatus{Semaphore: &wf.SemaphoreStatus{Waiting: []wf.SemaphoreHolding{{Semaphore: lock, Holders: []string{"runs/ended/one", "runs/ended/two"}}}}}}},
	}
	facts, err := argojob.LockFacts(t.Context(), rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range facts {
		if fact.Role == argojob.LockStale {
			if fact.Permits != 2 {
				t.Fatal("stale permits collapsed", fact)
			}
			return
		}
	}
	t.Fatal("stale holder missing")
}
