package main

import (
	"sync/atomic"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	wffake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktest "k8s.io/client-go/testing"
)

// A step's Pod can ask before Argo has saved its node. The broker answers a
// retryable 503, which the step client retries within its window, and
// delivers once the node is visible. A Pod naming no node stays forbidden.
func TestBrokerRetriesStepWhoseNodeIsNotSavedYet(t *testing.T) {
	f := newDeliveryFixture(t)
	var pending atomic.Bool
	pending.Store(true)
	f.o.cluster.Argo.(*wffake.Clientset).PrependReactor("get", "workflows", func(ktest.Action) (bool, runtime.Object, error) {
		if !pending.Load() {
			return false, nil, nil
		}
		w := f.w.DeepCopy()
		w.Status.Nodes = wf.Nodes{}
		return true, w, nil
	})
	h := f.handler(t)
	if r := deliveryRequest(t, h, f.jwt, `{"path":"registry/value"}`); r.Code != 503 || r.Header().Get("Retry-After") != "1" {
		t.Fatal("a step whose node is not saved yet was refused for good", r.Code)
	}
	pending.Store(false)
	if r := deliveryRequest(t, h, f.jwt, `{"path":"registry/value"}`); r.Code != 200 {
		t.Fatal("the step was not delivered once its node was visible", r.Code)
	}
	pending.Store(true)
	pod := f.pod.DeepCopy()
	delete(pod.Annotations, "workflows.argoproj.io/node-id")
	if _, err := f.o.cluster.Kube.CoreV1().Pods(pod.Namespace).Update(t.Context(), pod, meta.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if r := deliveryRequest(t, h, f.jwt, `{"path":"registry/value"}`); r.Code != 403 {
		t.Fatal("a Pod naming no node was answered retryably", r.Code)
	}
}
