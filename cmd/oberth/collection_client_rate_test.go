package main

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argofake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestCollectionClientsHaveASeparateSharedBudget(t *testing.T) {
	t.Parallel()
	clients, err := newClusterClients(&rest.Config{Host: "https://cluster.oberth.ci"})
	if err != nil {
		t.Fatal(err)
	}
	kube := clients.collectionKube.CoreV1().RESTClient().GetRateLimiter()
	argo := clients.collectionArgo.ArgoprojV1alpha1().RESTClient().GetRateLimiter()
	live := clients.Kube.CoreV1().RESTClient().GetRateLimiter()
	if kube != argo || kube == live || kube.QPS() != 5 || !kube.TryAccept() || argo.TryAccept() {
		t.Fatal("collection does not share its own 5 QPS / burst 1 client budget")
	}
	for range serverClientBurst {
		if !live.TryAccept() {
			t.Fatal("collection consumed the live client budget")
		}
	}
}

func TestCollectionClientsAreComposedIntoNativeExecution(t *testing.T) {
	t.Parallel()
	owner := &serverOwners{cluster: clusterClients{
		Kube: kubefake.NewClientset(), Argo: argofake.NewSimpleClientset(),
		collectionKube: kubefake.NewClientset(&core.Pod{ObjectMeta: meta.ObjectMeta{Name: "old-pod", Namespace: "runs", UID: "archive-pod"}}),
		collectionArgo: argofake.NewSimpleClientset(&wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: "old-workflow", Namespace: "runs", UID: "archive-workflow"}}),
	}}
	ports := owner.collectionClients("runs")
	pod, err := ports.Pods.Get(t.Context(), "old-pod", meta.GetOptions{})
	if err != nil || pod.UID != "archive-pod" {
		t.Fatal("collection used the live Pod client", pod, err)
	}
	workflow, err := ports.Workflows.Get(t.Context(), "old-workflow", meta.GetOptions{})
	if err != nil || workflow.UID != "archive-workflow" {
		t.Fatal("collection used the live Workflow client", workflow, err)
	}
}
