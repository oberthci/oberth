package argojob

import (
	"context"
	"slices"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
)

// The observation reports held and waited lock keys from the Workflow's own
// status.synchronization, nothing for a completed Workflow, and refuses a
// Workflow that is not the run's.
func TestObserveSynchronizationReadsNativeLockStatus(t *testing.T) {
	workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "wf", Namespace: testNamespace, Annotations: map[string]string{runIDAnnotation: "run-1"}},
		Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning, Synchronization: &wfv1.SynchronizationStatus{
			Semaphore: &wfv1.SemaphoreStatus{
				Holding: []wfv1.SemaphoreHolding{{Semaphore: testNamespace + "/ConfigMap/oberth-sync-limits/kvm", Holders: []string{"wf"}}},
				Waiting: []wfv1.SemaphoreHolding{{Semaphore: testNamespace + "/ConfigMap/oberth-sync-limits/heavy-go", Holders: []string{"other"}}},
			},
			Mutex: &wfv1.MutexStatus{Waiting: []wfv1.MutexHolding{{Mutex: testNamespace + "/Mutex/oberth-release", Holder: "other"}}},
		}}}
	controller, err := NewController(&staticWorkflowClient{workflow: workflow}, nil, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	observation, err := controller.ObserveSynchronization(context.Background(), "wf", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(observation.Holding, []string{model.SyncParallelismLock, testNamespace + "/ConfigMap/oberth-sync-limits/kvm"}) ||
		!slices.Equal(observation.Waiting, []string{testNamespace + "/ConfigMap/oberth-sync-limits/heavy-go", testNamespace + "/Mutex/oberth-release"}) {
		t.Fatalf("observation = %+v", observation)
	}
	if _, err := controller.ObserveSynchronization(context.Background(), "wf", "run-2"); err == nil {
		t.Fatal("foreign run accepted")
	}
	workflow.Status.Phase = wfv1.WorkflowSucceeded
	if observation, err := controller.ObserveSynchronization(context.Background(), "wf", "run-1"); err != nil || len(observation.Holding)+len(observation.Waiting) != 0 {
		t.Fatalf("completed workflow = %+v, %v", observation, err)
	}
}
