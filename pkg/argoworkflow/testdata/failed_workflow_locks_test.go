package sync

import (
	"context"
	"fmt"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Completed workflow cleanup calls ReleaseAll, including after failFast. Exercise
// that boundary with three holders sharing one backing slice, then demand all
// three permits from another workflow without restarting the manager.
func TestOberthFailedWorkflowReleasesEveryPermit(t *testing.T) {
	for _, kind := range []string{"semaphore", "mutex"} {
		t.Run(kind, func(t *testing.T) {
			ctx := logging.TestContext(t.Context())
			notified := false
			manager := NewLockManager(ctx, nil, "ci", nil,
				func(context.Context, string) (int, error) { return 3, nil }, func(name string) { notified = notified || name == "ci/next" }, func(string) bool { return true })
			wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "failed", Namespace: "ci"}}
			locks := []*wfv1.Synchronization{}
			for i := range 3 {
				lock := &wfv1.Synchronization{Semaphores: []*wfv1.SemaphoreRef{{ConfigMapKeyRef: &v1.ConfigMapKeySelector{
					LocalObjectReference: v1.LocalObjectReference{Name: "limits"}, Key: "heavy",
				}}}}
				if kind == "mutex" {
					lock = &wfv1.Synchronization{Mutexes: []*wfv1.Mutex{{Name: fmt.Sprintf("release-%d", i)}}}
				}
				locks = append(locks, lock)
				requirePermit(t, manager, wf, fmt.Sprintf("holder-%d", i), lock)
			}
			waiter := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "next", Namespace: "ci"}}
			for i, lock := range locks {
				acquired, _, _, _, err := manager.TryAcquire(ctx, waiter, fmt.Sprintf("waiter-%d", i), lock)
				if err != nil || acquired {
					t.Fatal("full lock admitted a waiter", err)
				}
			}
			wf.Status.Phase = wfv1.WorkflowFailed
			if !manager.ReleaseAll(ctx, wf) || wf.Status.Synchronization != nil {
				t.Fatal("failed workflow retained synchronization status")
			}
			for name, lock := range manager.syncLockMap {
				holders, err := lock.getCurrentHolders(ctx)
				if err != nil || len(holders) != 0 {
					t.Fatalf("failed workflow leaked holders for %s: %v, %v", name, holders, err)
				}
			}
			if !notified {
				t.Fatal("failed workflow cleanup did not wake the waiter")
			}
			for i, lock := range locks {
				requirePermit(t, manager, waiter, fmt.Sprintf("waiter-%d", i), lock)
			}
		})
	}
}

func requirePermit(t *testing.T, manager *Manager, wf *wfv1.Workflow, node string, lock *wfv1.Synchronization) {
	t.Helper()
	acquired, _, message, failedLock, err := manager.TryAcquire(logging.TestContext(t.Context()), wf, node, lock)
	if err != nil || !acquired {
		t.Fatalf("permit unavailable to %s/%s: %s %s %v", wf.Name, node, message, failedLock, err)
	}
}
