package argojob

import (
	"errors"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
)

// Scripted observation fixtures use the same cache interface as production,
// without a polling path or any client GET/LIST calls.
type testObservationHub struct {
	controller *Controller
	unhealthy  bool
	since      time.Time
}

func (h *testObservationHub) Healthy() bool {
	if h.unhealthy {
		return false
	}
	if client, ok := h.controller.workflows.(*schedulingWorkflowClient); ok {
		return client.err == nil || apierrors.IsNotFound(client.err)
	}
	return true
}
func (h *testObservationHub) UnhealthySince() (time.Time, bool) { return h.since, !h.Healthy() }
func (h *testObservationHub) Workflow(name string) (*wfv1.Workflow, bool) {
	var wf *wfv1.Workflow
	switch c := h.controller.workflows.(type) {
	case *schedulingWorkflowClient:
		if c.err == nil {
			wf = c.workflow
		}
	case *staticWorkflowClient:
		wf = c.workflow
	}
	if wf == nil {
		for _, pod := range h.Pods() {
			if pod.Labels[argoWorkflowLabel] == name {
				if owner := metav1.GetControllerOf(pod); owner != nil {
					return &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pod.Namespace, UID: owner.UID}}, true
				}
			}
		}
	}
	return wf, wf != nil
}
func (h *testObservationHub) Pods() []*corev1.Pod {
	client, ok := h.controller.kube.(*fake.Clientset)
	if !ok {
		return nil
	}
	obj, err := client.Tracker().List(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, h.controller.Namespace())
	if err != nil {
		panic(err)
	}
	var pods []*corev1.Pod
	for i := range obj.(*corev1.PodList).Items {
		pods = append(pods, &obj.(*corev1.PodList).Items[i])
	}
	return pods
}
func (h *testObservationHub) PodsOf(name string) []*corev1.Pod {
	var pods []*corev1.Pod
	for _, pod := range h.Pods() {
		if pod.Labels[argoWorkflowLabel] == name {
			pods = append(pods, pod)
		}
	}
	return pods
}
func withTestObservationCache(t *testing.T, controller *Controller) *Controller {
	t.Helper()
	if controller.hub == nil {
		controller.WithWatchHub(&testObservationHub{controller: controller})
	}
	return controller
}

func TestWatchObservationsNeverReadAPI(t *testing.T) {
	wf := schedulingWorkflow()
	pod := schedulingPod("pending", wf.UID)
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "Insufficient cpu"}}
	kube := fake.NewClientset(pod)
	client := &schedulingWorkflowClient{workflow: wf}
	controller := withTestObservationCache(t, &Controller{workflows: client, kube: kube, config: Config{Namespace: "pipeline", Assist: AssistConfig{Enabled: true}}})
	for range 100 {
		observation, err := controller.ObserveScheduling(t.Context(), "wf", "run", "sha")
		if err != nil || len(observation.Pods) != 1 || observation.Pods[0].Reason != "Unschedulable" {
			t.Fatal(observation, err)
		}
		if _, err := controller.ObserveSynchronization(t.Context(), "wf", "run"); err != nil {
			t.Fatal(err)
		}
		if _, err := controller.ListAssistHolds(t.Context(), "wf"); err != nil {
			t.Fatal(err)
		}
		if _, err := controller.EnforceAssistHolds(t.Context(), nil, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if client.gets != 0 || len(kube.Actions()) != 0 {
		t.Fatal("observation performed API reads", client.gets, kube.Actions())
	}
	hub := controller.hub.(*testObservationHub)
	hub.unhealthy = true
	hub.since = time.Date(2026, 11, 13, 12, 34, 0, 0, time.UTC)
	wf.Spec.Suspend = new(bool)
	*wf.Spec.Suspend = true
	observation, err := controller.ObserveScheduling(t.Context(), "wf", "run", "sha")
	if err != nil || observation.State != "unavailable" || observation.Message != "status unknown: cluster API unreachable since 12:34" || observation.Reason != "" || len(observation.Pods) != 0 {
		t.Fatal(observation, err)
	}
	if _, err := controller.EnforceAssistHolds(t.Context(), nil, nil, time.Now()); err == nil {
		t.Fatal("unhealthy sweep accepted")
	}
	if len(kube.Actions()) != 0 {
		t.Fatal(kube.Actions())
	}
}

func TestOrphanLivenessUsesWorkflowPhaseAndConfirmsCacheMiss(t *testing.T) {
	wf := schedulingWorkflow()
	client := &schedulingWorkflowClient{workflow: wf}
	controller := withTestObservationCache(t, &Controller{workflows: client, config: Config{Namespace: "pipeline"}})
	seeder := &SourceSeeder{hub: controller.hub, workflows: client}
	if !seeder.workflowLive(t.Context(), "wf") || client.gets != 0 {
		t.Fatal("running Workflow lost without Pods", client.gets)
	}
	wf.Status.Phase = wfv1.WorkflowSucceeded
	if seeder.workflowLive(t.Context(), "wf") || client.gets != 0 {
		t.Fatal("terminal phase ignored", client.gets)
	}
	hub := controller.hub.(*testObservationHub)
	hub.unhealthy = true
	if !seeder.workflowLive(t.Context(), "wf") || client.gets != 0 {
		t.Fatal("outage condemned source")
	}
	hub.unhealthy = false
	client.workflow = nil
	client.err = apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, "wf")
	if seeder.workflowLive(t.Context(), "wf") || client.gets != 1 {
		t.Fatal("cache miss not confirmed", client.gets)
	}
	client.err = errors.New("API unavailable")
	if !seeder.workflowLive(t.Context(), "wf") {
		t.Fatal("uncertainty condemned source")
	}
}

func TestAssistSweepConfirmsCachedVictimUIDBeforeDeleting(t *testing.T) {
	held := heldPodFixture("wf-test-1", "wf", "test", time.Hour, false)
	kube := fake.NewClientset(held)
	controller := assistSweepController(t, kube)
	// The cache keeps the old UID while the live API has a replacement.
	cached := &fixedObservationHub{testObservationHub: controller.hub.(*testObservationHub), pods: []*corev1.Pod{held.DeepCopy()}}
	controller.WithWatchHub(cached)
	replacement := held.DeepCopy()
	replacement.UID = "replacement-uid"
	if _, err := kube.CoreV1().Pods(testNamespace).Update(t.Context(), replacement, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	abandoned, err := controller.EnforceAssistHolds(t.Context(), nil, nil, time.Now())
	if err != nil || len(abandoned) != 0 {
		t.Fatal(abandoned, err)
	}
	for _, action := range kube.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatal("deleted replacement")
		}
	}
}

type fixedObservationHub struct {
	*testObservationHub
	pods []*corev1.Pod
}

func (h *fixedObservationHub) Pods() []*corev1.Pod { return h.pods }

func TestOwnsUsesCacheAndConfirmsMiss(t *testing.T) {
	client := &schedulingWorkflowClient{workflow: schedulingWorkflow()}
	controller := withTestObservationCache(t, &Controller{workflows: client, config: Config{Namespace: "pipeline"}})
	if owned, err := controller.Owns(t.Context(), "wf"); err != nil || !owned || client.gets != 0 {
		t.Fatal(owned, err, client.gets)
	}
	client.workflow = nil
	client.err = apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, "wf")
	if owned, err := controller.Owns(t.Context(), "wf"); err != nil || owned || client.gets != 1 {
		t.Fatal(owned, err, client.gets)
	}
}

func TestAssistSweepObservesHealthyHeldPodWithoutAPIReads(t *testing.T) {
	held := heldPodFixture("wf-test-1", "wf", "test", time.Minute, false)
	kube := fake.NewClientset(held)
	controller := assistSweepController(t, kube)
	for range 100 {
		holds, err := controller.ListAssistHolds(t.Context(), "wf")
		if err != nil || len(holds) != 1 {
			t.Fatal(holds, err)
		}
		abandoned, err := controller.EnforceAssistHolds(t.Context(), nil, nil, time.Now())
		if err != nil || len(abandoned) != 0 {
			t.Fatal(abandoned, err)
		}
	}
	if len(kube.Actions()) != 0 {
		t.Fatal("healthy assist observation performed API I/O", kube.Actions())
	}
}
