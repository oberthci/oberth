package argowatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argofake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

const testNamespace = "pipeline"

func workflowObject(name string) *wfv1.Workflow {
	return &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: testNamespace, UID: types.UID(name + "-uid"), ResourceVersion: "opaque-initial",
		Labels:      map[string]string{"oberth.ci/engine": "argo"},
		Annotations: map[string]string{"oberth.ci/run-id": name + "-run"},
	}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowPending}}
}

func podObject(name, workflow string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: testNamespace, UID: types.UID(name + "-uid"), ResourceVersion: "opaque-initial",
		Labels: map[string]string{"oberth.ci/trigger": "push", workflowLabel: workflow},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow",
			Name: workflow, UID: types.UID(workflow + "-uid"), Controller: ptr.To(true)}},
	}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
}

// Argo's generated fake predates the client-go capability method. Model the
// API's unsupported streaming-list response, letting the reflector fall back.
func rejectStreamingList(action clienttesting.Action) (bool, watch.Interface, error) {
	options := action.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
	if options.SendInitialEvents != nil && *options.SendInitialEvents {
		return true, nil, apierrors.NewBadRequest("streaming lists unsupported")
	}
	return false, nil, nil
}

func fakeArgo(objects ...runtime.Object) *argofake.Clientset {
	client := argofake.NewSimpleClientset(objects...)
	client.PrependWatchReactor("workflows", rejectStreamingList)
	return client
}

func newTestHub(t *testing.T, kube *kubefake.Clientset, argo *argofake.Clientset) *Hub {
	t.Helper()
	h, err := New(kube, argo, testNamespace, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func runHub(t *testing.T, h *Hub) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not stop")
		}
		// The Argo factory has no Shutdown method. Await its informer as well.
		eventually(t, func() bool { return h.workflows.IsStopped() }, "Workflow informer shutdown")
	})
	return cancel
}

func syncHub(t *testing.T, h *Hub) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := h.WaitForSync(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.Healthy() {
		t.Fatal("synced hub is unhealthy")
	}
}

func eventually(t *testing.T, condition func() bool, what string) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out: %s", what)
		case <-tick.C:
		}
	}
}

func awaitWake(t *testing.T, wake <-chan struct{}) {
	t.Helper()
	select {
	case <-wake:
	case <-time.After(15 * time.Second):
		t.Fatal("missing wake")
	}
}

func noWake(t *testing.T, wake <-chan struct{}) {
	t.Helper()
	select {
	case <-wake:
		t.Fatal("unexpected wake")
	default:
	}
}

func TestSyncSelectorsTransformsAndPodIndex(t *testing.T) {
	workflow := workflowObject("one")
	workflow.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "controller"}}
	workflow.Spec.Entrypoint = "entry"
	workflow.Spec.Templates = []wfv1.Template{{Name: "entry", Container: &corev1.Container{Image: "step"}}}
	workflow.Status.Nodes = wfv1.Nodes{"node": {ID: "node", Phase: wfv1.NodePending}}
	workflow.Status.StoredTemplates = map[string]wfv1.Template{"stored": {Name: "stored"}}
	workflow.Status.StoredWorkflowSpec = &wfv1.WorkflowSpec{Entrypoint: "stored"}
	pod := podObject("step-one", "one")
	pod.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "controller"}}
	pod.Spec.Containers = []corev1.Container{{Name: "main", Image: "step"}}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable"}}
	argo, kube := fakeArgo(workflow), kubefake.NewSimpleClientset(pod, podObject("step-two", "two"))
	h := newTestHub(t, kube, argo)
	if h.Healthy() {
		t.Fatal("hub healthy before sync")
	}
	if since, unavailable := h.UnhealthySince(); !unavailable || since.IsZero() {
		t.Fatal("startup unavailability must have a timestamp")
	}
	runHub(t, h)
	syncHub(t, h)
	got, ok := h.Workflow("one")
	if !ok || len(got.ManagedFields) != 0 || got.Status.StoredTemplates != nil || got.Status.StoredWorkflowSpec != nil {
		t.Fatalf("Workflow transform: %#v", got)
	}
	if !reflect.DeepEqual(got.Spec, workflow.Spec) || !reflect.DeepEqual(got.Status.Nodes, workflow.Status.Nodes) {
		t.Fatal("Workflow spec or node status changed")
	}
	gotPod, ok := h.Pod("step-one")
	if !ok || len(gotPod.ManagedFields) != 0 || !reflect.DeepEqual(gotPod.Spec, pod.Spec) || !reflect.DeepEqual(gotPod.Status, pod.Status) {
		t.Fatalf("Pod transform: %#v", gotPod)
	}
	if pods := h.PodsOf("one"); len(pods) != 1 || pods[0] != gotPod {
		t.Fatalf("unexpected Pod index: %#v", pods)
	}
	if len(h.PodsOf("two")) != 1 || len(h.PodsOf("missing")) != 0 || len(h.PodsOf("")) != 0 {
		t.Fatal("Pod index mixed Workflows")
	}
	if other, _ := h.Workflow("one"); other != got {
		t.Fatal("Workflow read copied the shared cache object")
	}
	if _, ok := h.Workflow("missing"); ok {
		t.Fatal("missing Workflow found")
	}
	if _, ok := h.Pod("missing"); ok {
		t.Fatal("missing Pod found")
	}
	original, _ := argo.ArgoprojV1alpha1().Workflows(testNamespace).Get(context.Background(), "one", metav1.GetOptions{})
	if len(original.ManagedFields) != 1 || original.Status.StoredTemplates == nil || original.Status.StoredWorkflowSpec == nil {
		t.Fatal("transform mutated the API object")
	}
	for _, actions := range [][]clienttesting.Action{argo.Actions(), kube.Actions()} {
		var lists, watches int
		for _, action := range actions {
			if action.GetVerb() != "list" && action.GetVerb() != "watch" {
				continue
			}
			if action.GetNamespace() != testNamespace {
				t.Fatalf("not namespace scoped: %#v", action)
			}
			want := workflowSelector
			if action.GetResource().Resource == "pods" {
				want = podSelector
			}
			var selector string
			if action.GetVerb() == "list" {
				lists++
				selector = action.(clienttesting.ListAction).GetListRestrictions().Labels.String()
			} else {
				watches++
				selector = action.(clienttesting.WatchAction).GetWatchRestrictions().Labels.String()
			}
			if selector != want {
				t.Fatalf("%s selector = %q, want %q", action.GetVerb(), selector, want)
			}
		}
		if lists == 0 || watches == 0 {
			t.Fatal("expected both LIST and WATCH")
		}
	}
}

func TestSubscribeBeforeCreateAndEventFanout(t *testing.T) {
	for _, kind := range []string{"Workflow", "Pod"} {
		t.Run(kind, func(t *testing.T) {
			argo, kube := fakeArgo(), kubefake.NewSimpleClientset()
			h := newTestHub(t, kube, argo)
			wake, cancel := h.Subscribe("one")
			defer cancel()
			second, cancelSecond := h.Subscribe("one")
			defer cancelSecond()
			other, cancelOther := h.Subscribe("two")
			defer cancelOther()
			runHub(t, h)
			syncHub(t, h)
			ctx := context.Background()
			if kind == "Workflow" {
				client := argo.ArgoprojV1alpha1().Workflows(testNamespace)
				obj, err := client.Create(ctx, workflowObject("one"), metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
				awaitWake(t, wake)
				awaitWake(t, second)
				noWake(t, other)
				if _, ok := h.Workflow("one"); !ok {
					t.Fatal("wake before cache add")
				}
				obj.Status.Phase, obj.ResourceVersion = wfv1.WorkflowRunning, "opaque-next"
				if _, err := client.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				awaitWake(t, wake)
				awaitWake(t, second)
				noWake(t, other)
				if got, _ := h.Workflow("one"); got.Status.Phase != wfv1.WorkflowRunning {
					t.Fatal("stale Workflow after update")
				}
				if err := client.Delete(ctx, "one", metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
				awaitWake(t, wake)
				awaitWake(t, second)
				noWake(t, other)
				if _, ok := h.Workflow("one"); ok {
					t.Fatal("Workflow remains after delete")
				}
			} else {
				client := kube.CoreV1().Pods(testNamespace)
				obj, err := client.Create(ctx, podObject("step", "one"), metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
				awaitWake(t, wake)
				awaitWake(t, second)
				noWake(t, other)
				if _, ok := h.Pod("step"); !ok {
					t.Fatal("wake before cache add")
				}
				obj.Status.Phase, obj.ResourceVersion = corev1.PodRunning, "opaque-next"
				if _, err := client.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				awaitWake(t, wake)
				awaitWake(t, second)
				noWake(t, other)
				if got, _ := h.Pod("step"); got.Status.Phase != corev1.PodRunning {
					t.Fatal("stale Pod after update")
				}
				if err := client.Delete(ctx, "step", metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
				awaitWake(t, wake)
				awaitWake(t, second)
				noWake(t, other)
				if _, ok := h.Pod("step"); ok || len(h.PodsOf("one")) != 0 {
					t.Fatal("Pod remains after delete")
				}
			}
		})
	}
}

func TestCoalescingReadsFinalCacheState(t *testing.T) {
	argo, kube := fakeArgo(), kubefake.NewSimpleClientset()
	h := newTestHub(t, kube, argo)
	wake, cancel := h.Subscribe("one")
	defer cancel()
	barrier, cancelBarrier := h.Subscribe("barrier")
	defer cancelBarrier()
	runHub(t, h)
	syncHub(t, h)
	client := argo.ArgoprojV1alpha1().Workflows(testNamespace)
	obj, err := client.Create(context.Background(), workflowObject("one"), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		obj.ResourceVersion = fmt.Sprintf("opaque-%d", i)
		obj.Status.Message = fmt.Sprintf("update-%d", i)
		if _, err := client.Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.Create(context.Background(), workflowObject("barrier"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	awaitWake(t, barrier) // All preceding updates reached this informer's handler.
	if len(wake) != 1 {
		t.Fatalf("coalesced pending wakes = %d", len(wake))
	}
	awaitWake(t, wake)
	if got, _ := h.Workflow("one"); got.Status.Message != "update-99" {
		t.Fatal("did not observe final state")
	}
	noWake(t, wake)
}

func TestCancelRemovesSubscriptionsWithoutLeaks(t *testing.T) {
	h := newTestHub(t, kubefake.NewSimpleClientset(), fakeArgo())
	var wg sync.WaitGroup
	for i := range 1000 {
		wg.Go(func() {
			name := fmt.Sprintf("workflow-%d", i%10)
			_, cancel := h.Subscribe(name)
			h.notify(name)
			cancel()
			cancel()
		})
	}
	wg.Wait()
	h.subMu.Lock()
	defer h.subMu.Unlock()
	if len(h.subs) != 0 {
		t.Fatalf("leaked subscriptions: %d", len(h.subs))
	}
}

func TestTombstoneDelete(t *testing.T) {
	h := newTestHub(t, kubefake.NewSimpleClientset(), fakeArgo())
	wake, cancel := h.Subscribe("one")
	defer cancel()
	for _, item := range []struct {
		handler cache.ResourceEventHandlerFuncs
		object  any
	}{
		{h.handlers(h.workflowName), workflowObject("one")},
		{h.handlers(h.podWorkflowName), podObject("step", "one")},
	} {
		item.handler.DeleteFunc(cache.DeletedFinalStateUnknown{Key: testNamespace + "/one", Obj: item.object})
		awaitWake(t, wake)
		item.handler.DeleteFunc(&cache.DeletedFinalStateUnknown{Key: testNamespace + "/one", Obj: item.object})
		awaitWake(t, wake)
		item.handler.DeleteFunc(cache.DeletedFinalStateUnknown{Key: testNamespace + "/one", Obj: "bad-object"})
		item.handler.DeleteFunc(nil)
		noWake(t, wake)
	}
}

func TestForeignObjectsIgnored(t *testing.T) {
	argo, kube := fakeArgo(), kubefake.NewSimpleClientset()
	h := newTestHub(t, kube, argo)
	runHub(t, h)
	syncHub(t, h)
	workflowCases := []struct {
		name   string
		change func(*wfv1.Workflow)
	}{
		{"unlabelled", func(w *wfv1.Workflow) { w.Labels = nil }},
		{"other-engine", func(w *wfv1.Workflow) { w.Labels["oberth.ci/engine"] = "other" }},
		{"no-run-id", func(w *wfv1.Workflow) { w.Annotations = nil }},
	}
	for _, item := range workflowCases {
		obj := workflowObject(item.name)
		item.change(obj)
		wake, cancel := h.Subscribe(obj.Name)
		if _, err := argo.ArgoprojV1alpha1().Workflows(testNamespace).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		eventually(t, func() bool { _, ok, _ := h.workflows.GetStore().GetByKey(testNamespace + "/" + obj.Name); return ok }, "foreign Workflow delivered by unfiltered fake watch")
		marker, cancelMarker := h.Subscribe("marker-" + item.name)
		if _, err := argo.ArgoprojV1alpha1().Workflows(testNamespace).Create(context.Background(), workflowObject("marker-"+item.name), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		awaitWake(t, marker)
		if _, ok := h.Workflow(obj.Name); ok {
			t.Fatalf("foreign Workflow %s exposed", item.name)
		}
		noWake(t, wake)
		cancel()
		cancelMarker()
	}
	podCases := []struct {
		name   string
		change func(*corev1.Pod)
	}{
		{"unlabelled", func(p *corev1.Pod) { delete(p.Labels, "oberth.ci/trigger") }},
		{"no-workflow", func(p *corev1.Pod) { delete(p.Labels, workflowLabel) }},
		{"no-owner", func(p *corev1.Pod) { p.OwnerReferences = nil }},
		{"wrong-owner-name", func(p *corev1.Pod) { p.OwnerReferences[0].Name = "other" }},
		{"wrong-owner-kind", func(p *corev1.Pod) { p.OwnerReferences[0].Kind = "Pod" }},
		{"wrong-owner-api", func(p *corev1.Pod) { p.OwnerReferences[0].APIVersion = "v1" }},
		{"no-owner-uid", func(p *corev1.Pod) { p.OwnerReferences[0].UID = "" }},
		{"non-controller", func(p *corev1.Pod) { p.OwnerReferences[0].Controller = ptr.To(false) }},
	}
	for _, item := range podCases {
		obj := podObject(item.name, item.name)
		item.change(obj)
		wake, cancel := h.Subscribe(item.name)
		if _, err := kube.CoreV1().Pods(testNamespace).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		eventually(t, func() bool { _, ok, _ := h.pods.GetStore().GetByKey(testNamespace + "/" + obj.Name); return ok }, "foreign Pod delivered by unfiltered fake watch")
		marker, cancelMarker := h.Subscribe("marker-" + item.name)
		if _, err := kube.CoreV1().Pods(testNamespace).Create(context.Background(), podObject("marker-"+item.name, "marker-"+item.name), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		awaitWake(t, marker)
		if _, ok := h.Pod(obj.Name); ok || len(h.PodsOf(item.name)) != 0 {
			t.Fatalf("foreign Pod %s exposed", item.name)
		}
		noWake(t, wake)
		cancel()
		cancelMarker()
	}
}

func TestPodReindexAndOwnershipLossWakeOldSubscriber(t *testing.T) {
	argo, kube := fakeArgo(), kubefake.NewSimpleClientset()
	h := newTestHub(t, kube, argo)
	runHub(t, h)
	syncHub(t, h)
	one, cancelOne := h.Subscribe("one")
	defer cancelOne()
	two, cancelTwo := h.Subscribe("two")
	defer cancelTwo()
	client := kube.CoreV1().Pods(testNamespace)
	obj, err := client.Create(context.Background(), podObject("step", "one"), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	awaitWake(t, one)
	noWake(t, two)
	obj.Labels[workflowLabel] = "two"
	obj.OwnerReferences[0].Name, obj.OwnerReferences[0].UID = "two", "two-uid"
	obj.ResourceVersion = "changed-owner"
	if _, err := client.Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	awaitWake(t, one)
	awaitWake(t, two)
	if len(h.PodsOf("one")) != 0 || len(h.PodsOf("two")) != 1 {
		t.Fatal("Pod index did not follow Workflow label")
	}
	delete(obj.Labels, "oberth.ci/trigger")
	obj.ResourceVersion = "lost-ownership"
	if _, err := client.Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	awaitWake(t, two)
	noWake(t, one)
	if _, ok := h.Pod("step"); ok || len(h.PodsOf("two")) != 0 {
		t.Fatal("unowned Pod still exposed")
	}
}

func TestWaitForSyncCancellation(t *testing.T) {
	h := newTestHub(t, kubefake.NewSimpleClientset(), fakeArgo())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.WaitForSync(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitForSync = %v", err)
	}
}
