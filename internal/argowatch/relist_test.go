package argowatch

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestRelistOn410ConvergesAndWakesDeletedAndChanged(t *testing.T) {
	for _, kind := range []string{"Workflow", "Pod"} {
		t.Run(kind, func(t *testing.T) {
			argo := fakeArgo(workflowObject("deleted"), workflowObject("changed"))
			kube := kubefake.NewSimpleClientset(podObject("deleted", "deleted"), podObject("changed", "changed"))
			h := newTestHub(t, kube, argo)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream := watch.NewRaceFreeFake()
			defer stream.Stop()
			watchStarted, relistStarted, gapDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var firstWatch, firstRelist sync.Once
			var listCount atomic.Int32
			listReactor := func(clienttesting.Action) (bool, runtime.Object, error) {
				if listCount.Add(1) > 1 {
					firstRelist.Do(func() { close(relistStarted) })
					select {
					case <-gapDone:
					case <-ctx.Done():
						return true, nil, ctx.Err()
					}
				}
				return false, nil, nil
			}
			watchReactor := func(action clienttesting.Action) (bool, watch.Interface, error) {
				if handled, result, err := rejectStreamingList(action); handled {
					return handled, result, err
				}
				first := false
				firstWatch.Do(func() { first = true; close(watchStarted) })
				if first {
					return true, stream, nil
				}
				return false, nil, nil
			}
			tracker := argo.Tracker()
			resource := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "workflows"}
			if kind == "Workflow" {
				argo.PrependReactor("list", "workflows", listReactor)
				argo.PrependWatchReactor("workflows", watchReactor)
			} else {
				kube.PrependReactor("list", "pods", listReactor)
				kube.PrependWatchReactor("pods", watchReactor)
				tracker = kube.Tracker()
				resource = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
			}
			runHub(t, h)
			syncHub(t, h)
			awaitClosed(t, watchStarted, "initial watch")
			// Markers pass through both real informer handlers so no initial
			// event can be mistaken for a wake caused by the later relist.
			workflowMarker, cancelWorkflowMarker := h.Subscribe("workflow-marker")
			defer cancelWorkflowMarker()
			podMarker, cancelPodMarker := h.Subscribe("pod-marker")
			defer cancelPodMarker()
			if kind == "Workflow" {
				stream.Add(workflowObject("workflow-marker"))
				if _, err := kube.CoreV1().Pods(testNamespace).Create(ctx, podObject("pod-marker", "pod-marker"), metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			} else {
				stream.Add(podObject("pod-marker", "pod-marker"))
				if _, err := argo.ArgoprojV1alpha1().Workflows(testNamespace).Create(ctx, workflowObject("workflow-marker"), metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			awaitWake(t, workflowMarker)
			awaitWake(t, podMarker)
			deleted, cancelDeleted := h.Subscribe("deleted")
			defer cancelDeleted()
			changed, cancelChanged := h.Subscribe("changed")
			defer cancelChanged()
			stream.Error(&metav1.Status{Status: metav1.StatusFailure, Code: 410, Reason: metav1.StatusReasonExpired})
			awaitClosed(t, relistStarted, "relist after 410")
			// The expired stream has stopped and the relist is held open. Mutate
			// only the API tracker, so these changes have no watch delivery.
			if err := tracker.Delete(resource, testNamespace, "deleted"); err != nil {
				t.Fatal(err)
			}
			var updated runtime.Object
			if kind == "Workflow" {
				obj := workflowObject("changed")
				obj.Status.Phase, obj.ResourceVersion, obj.UID = wfv1.WorkflowSucceeded, "opaque-after-gap", "replacement-uid"
				updated = obj
			} else {
				obj := podObject("changed", "changed")
				obj.Status.Phase, obj.ResourceVersion, obj.UID = corev1.PodSucceeded, "opaque-after-gap", "replacement-uid"
				updated = obj
			}
			if err := tracker.Update(resource, updated, testNamespace); err != nil {
				t.Fatal(err)
			}
			close(gapDone)
			eventually(t, func() bool {
				if kind == "Workflow" {
					_, exists := h.Workflow("deleted")
					obj, ok := h.Workflow("changed")
					return !exists && ok && obj.Status.Phase == wfv1.WorkflowSucceeded && obj.UID == "replacement-uid"
				}
				_, exists := h.Pod("deleted")
				obj, ok := h.Pod("changed")
				return !exists && ok && obj.Status.Phase == corev1.PodSucceeded && obj.UID == "replacement-uid" && len(h.PodsOf("deleted")) == 0
			}, "cache converged after relist")
			awaitWake(t, deleted)
			awaitWake(t, changed)
			if listCount.Load() < 2 {
				t.Fatal("no relist occurred")
			}
		})
	}
}

func awaitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	eventually(t, func() bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}, what)
}
