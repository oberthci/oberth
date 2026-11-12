package argojob

import (
	"errors"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestLivePodRequestsRetainsPendingTerminatingAndSiblingPods(t *testing.T) {
	config := testConfig()
	request := testRequest(pipeline.TriggerCI, greedyDocument)
	wf, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	wf.UID = "owned-workflow"
	pod := func(name, cpu string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: config.Namespace, Labels: map[string]string{argoWorkflowLabel: wf.Name}, OwnerReferences: []metav1.OwnerReference{{Kind: "Workflow", UID: wf.UID, Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}}}}}, Status: corev1.PodStatus{Phase: phase}}
	}
	held, pending, terminating, done, foreign := pod("held", "1", corev1.PodRunning), pod("sibling", "2", corev1.PodPending), pod("terminating", "3", corev1.PodRunning), pod("done", "8", corev1.PodSucceeded), pod("foreign", "8", corev1.PodRunning)
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"example.test/cleanup"}
	foreign.OwnerReferences[0].UID = "another-workflow"
	client := fake.NewClientset(held, pending, terminating, done, foreign)
	controller := &Controller{kube: client, workflows: &staticWorkflowClient{workflow: wf}, config: config}
	live, err := controller.LivePodRequests(t.Context(), wf.Name, request.RunID)
	if err != nil || live.CPUMilli != 6000 || live.Pods != 3 {
		t.Fatalf("resident requests lost: %+v %v", live, err)
	}
	if _, err = controller.LivePodRequests(t.Context(), wf.Name, "different-run"); err == nil {
		t.Fatal("unowned workflow supplied CPU credit")
	}
}

func TestLivePodRequestsSynchronizationWaitHasZeroResidentRequests(t *testing.T) {
	config := testConfig()
	request := testRequest(pipeline.TriggerCI, greedyDocument)
	wf, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	wf.UID = "owned-workflow"
	wf.Status.Phase = wfv1.WorkflowRunning
	wf.Status.Synchronization = &wfv1.SynchronizationStatus{Mutex: &wfv1.MutexStatus{Waiting: []wfv1.MutexHolding{{Mutex: config.Namespace + "/Mutex/release", Holder: "other"}}}}
	client := fake.NewClientset()
	controller := &Controller{kube: client, workflows: &staticWorkflowClient{workflow: wf}, config: config}
	live, err := controller.LivePodRequests(t.Context(), wf.Name, request.RunID)
	if err != nil || live == nil || live.CPUMilli != 0 || live.Pods != 0 {
		t.Fatalf("synchronization wait=%+v %v", live, err)
	}
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("read denied") })
	if _, err := controller.LivePodRequests(t.Context(), wf.Name, request.RunID); err == nil {
		t.Fatal("failed Pod read became zero credit")
	}
}
