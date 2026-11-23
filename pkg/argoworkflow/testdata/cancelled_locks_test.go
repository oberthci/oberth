package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/sync"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
)

// lockCase is one run whose steps a and b hold both units of a two-unit
// semaphore while c and d wait, ended by mode ("" lets every step fail on its
// own).
type lockCase struct {
	key   string
	mode  wfv1.ShutdownStrategy
	retry bool
}

// The pinned controller's lock bookkeeping around a cancelled run. Each case
// ends one run as described above, then a later run with three steps asks for
// the same semaphore; then an ordinary hand-over of a one-unit lock; then runs
// waiting for a workflow-level mutex are stopped or terminated. The Workflows are written as the controller left them,
// reduced to the fields lock status and run status read.
func TestOberthCancelledRunLocks(t *testing.T) {
	ctx := logging.TestContext(t.Context())
	cancel, controller := newController(ctx, defaultServiceAccount, func(c *WorkflowController) {
		c.eventRecorderManager = &testEventRecorderManager{eventRecorder: record.NewFakeRecorder(10000)}
	})
	defer cancel()
	controller.syncManager = sync.NewLockManager(ctx, controller.kubeclientset, controller.namespace, nil, getSyncLimitFunc(ctx, controller.kubeclientset), func(string) {}, workflowExistenceFunc)
	cases := []lockCase{{key: "heavy-stop", mode: wfv1.ShutdownStrategyStop}, {key: "heavy-terminate", mode: wfv1.ShutdownStrategyTerminate}, {key: "heavy-retry", mode: wfv1.ShutdownStrategyStop, retry: true}, {key: "heavy-failed"}}
	limits := &apiv1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "oberth-sync-limits", Namespace: "default"}, Data: map[string]string{}}
	for _, c := range cases {
		limits.Data[c.key] = "2"
	}
	limits.Data["handover"] = "1"
	if _, err := controller.kubeclientset.CoreV1().ConfigMaps("default").Create(ctx, limits, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	observed := []wfv1.Workflow{}
	for _, c := range cases {
		holder := lockRun(ctx, t, controller, semaphoreWorkflow("ended-"+c.key, c, "a", "b", "c", "d"))
		makePodsPhase(ctx, newWorkflowOperationCtx(ctx, holder, controller), apiv1.PodRunning)
		holder = endRun(ctx, t, controller, lockRun(ctx, t, controller, holder).Name, c.mode)
		waiter := lockRun(ctx, t, controller, semaphoreWorkflow("waiting-"+c.key, lockCase{key: c.key}, "e", "f", "g"))
		observed = append(observed, *reduced(holder), *reduced(waiter))
		checkRestartPreservesHolds(ctx, t, controller, c)
	}
	observed = append(observed, handOver(ctx, t, controller)...)
	first := lockRun(ctx, t, controller, mutexWorkflow("mutex-holder"))
	for _, c := range []lockCase{{key: "mutex-pending-stop", mode: wfv1.ShutdownStrategyStop}, {key: "mutex-pending-terminate", mode: wfv1.ShutdownStrategyTerminate}} {
		pending := lockRun(ctx, t, controller, mutexWorkflow(c.key))
		observed = append(observed, *reduced(endRun(ctx, t, controller, pending.Name, c.mode)))
	}
	observed = append(observed, *reduced(first))
	raw, err := json.MarshalIndent(observed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("OBERTH_CONTROLLER_CASES"), "cancelled-locks.json"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

// handOver is the ordinary case beside the leak: a one-unit lock held by
// handover-a, which ends by itself, while handover-b and handover-c wait. Only
// the head of the queue, b, is woken and takes the unit; c is not reconciled
// again. Nothing leaks: a restarted controller has no unit either.
func handOver(ctx context.Context, t *testing.T, controller *WorkflowController) []wfv1.Workflow {
	t.Helper()
	c := lockCase{key: "handover"}
	a := lockRun(ctx, t, controller, semaphoreWorkflow("handover-a", c, "a"))
	makePodsPhase(ctx, newWorkflowOperationCtx(ctx, a, controller), apiv1.PodRunning)
	a = lockRun(ctx, t, controller, a)
	b := lockRun(ctx, t, controller, semaphoreWorkflow("handover-b", c, "b"))
	waiting := lockRun(ctx, t, controller, semaphoreWorkflow("handover-c", c, "c"))
	makePodsPhase(ctx, newWorkflowOperationCtx(ctx, a, controller), apiv1.PodSucceeded)
	for range 3 {
		if a = lockRun(ctx, t, controller, a); a.Status.Fulfilled() {
			break
		}
	}
	b = lockRun(ctx, t, controller, b)
	checkRestartPreservesHolds(ctx, t, controller, c)
	return []wfv1.Workflow{*reduced(a), *reduced(b), *reduced(waiting)}
}

// Normal shutdown releases node locks before final cleanup in the pinned
// controller. Both live and restarted managers must count all later holders.
func checkRestartPreservesHolds(ctx context.Context, t *testing.T, controller *WorkflowController, c lockCase) {
	t.Helper()
	rows, err := controller.wfclientset.ArgoprojV1alpha1().Workflows("default").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fresh := sync.NewLockManager(ctx, controller.kubeclientset, controller.namespace, nil, getSyncLimitFunc(ctx, controller.kubeclientset), func(string) {}, workflowExistenceFunc)
	if _, err := fresh.Initialize(ctx, rows.Items); err != nil {
		t.Fatal(err)
	}
	probe := semaphoreWorkflow("probe-"+c.key, c, "p")
	ref := probe.Spec.Templates[1].Synchronization
	running, _, _, _, err := controller.syncManager.TryAcquire(ctx, probe.DeepCopy(), "probe", ref)
	if err != nil || running {
		t.Fatal("running controller had a free unit", c.key, err)
	}
	restarted, _, _, _, err := fresh.TryAcquire(ctx, probe.DeepCopy(), "probe", ref)
	if err != nil || restarted {
		t.Fatal("restart lost a recorded live holder", c.key, restarted, err)
	}
}

// lockRun reconciles the stored Workflow once and returns what the controller stored.
func lockRun(ctx context.Context, t *testing.T, controller *WorkflowController, w *wfv1.Workflow) *wfv1.Workflow {
	t.Helper()
	client := controller.wfclientset.ArgoprojV1alpha1().Workflows("default")
	stored, err := client.Get(ctx, w.Name, metav1.GetOptions{})
	if err != nil {
		if stored, err = client.Create(ctx, w, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	newWorkflowOperationCtx(ctx, stored, controller).operate(ctx)
	if stored, err = client.Get(ctx, w.Name, metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	return stored
}

// endRun patches the strategy as Oberth's cancel does (none for ""), then
// reconciles until the controller records the Workflow as ended, failing its
// Pods as terminated or failing containers would. A Workflow that is still
// waiting after six passes is returned as the controller keeps it.
func endRun(ctx context.Context, t *testing.T, controller *WorkflowController, name string, mode wfv1.ShutdownStrategy) *wfv1.Workflow {
	t.Helper()
	client := controller.wfclientset.ArgoprojV1alpha1().Workflows("default")
	w, err := client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mode != "" {
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"shutdown": mode}})
		if w, err = client.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for range 6 {
		if w = lockRun(ctx, t, controller, w); w.Status.Fulfilled() {
			return w
		}
		makePodsPhase(ctx, newWorkflowOperationCtx(ctx, w, controller), apiv1.PodFailed, withExitCode(143))
	}
	return w
}

func oberthLabels(w *wfv1.Workflow) *wfv1.Workflow {
	w.Namespace = "default"
	w.Labels = map[string]string{"oberth.ci/engine": "argo", "oberth.ci/repo": "1", "oberth.ci/run": w.Name}
	w.Annotations = map[string]string{"oberth.ci/run-id": w.Name}
	return w
}

func semaphoreWorkflow(name string, c lockCase, tasks ...string) *wfv1.Workflow {
	dag := &wfv1.DAGTemplate{}
	for _, task := range tasks {
		dag.Tasks = append(dag.Tasks, wfv1.DAGTask{Name: task, Template: "heavy"})
	}
	heavy := wfv1.Template{Name: "heavy", Container: &apiv1.Container{Image: "busybox", Command: []string{"sleep", "600"}}}
	heavy.Synchronization = &wfv1.Synchronization{Semaphores: []*wfv1.SemaphoreRef{{ConfigMapKeyRef: &apiv1.ConfigMapKeySelector{LocalObjectReference: apiv1.LocalObjectReference{Name: "oberth-sync-limits"}, Key: c.key}}}}
	if c.retry {
		limit := intstr.FromInt32(1)
		heavy.RetryStrategy = &wfv1.RetryStrategy{Limit: &limit, RetryPolicy: wfv1.RetryPolicyAlways}
	}
	return oberthLabels(&wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: wfv1.WorkflowSpec{Entrypoint: "main", Templates: []wfv1.Template{{Name: "main", DAG: dag}, heavy}}})
}

func mutexWorkflow(name string) *wfv1.Workflow {
	return oberthLabels(&wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: wfv1.WorkflowSpec{Entrypoint: "main", Synchronization: &wfv1.Synchronization{Mutexes: []*wfv1.Mutex{{Name: "release"}}}, Templates: []wfv1.Template{
		{Name: "main", Container: &apiv1.Container{Image: "busybox", Command: []string{"sleep", "600"}}},
	}}})
}

// reduced keeps identity, strategy, phase, lock status and node lock status;
// holder lists are sorted because the controller reads them from a map.
func reduced(w *wfv1.Workflow) *wfv1.Workflow {
	out := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: w.Name, Namespace: w.Namespace, Labels: w.Labels, Annotations: w.Annotations}, Spec: wfv1.WorkflowSpec{Shutdown: w.Spec.Shutdown}, Status: wfv1.WorkflowStatus{Phase: w.Status.Phase, Synchronization: w.Status.Synchronization.DeepCopy()}}
	if s := out.Status.Synchronization; s != nil && s.Semaphore != nil {
		for _, list := range [][]wfv1.SemaphoreHolding{s.Semaphore.Holding, s.Semaphore.Waiting} {
			for i := range list {
				slices.Sort(list[i].Holders)
			}
		}
	}
	for id, n := range w.Status.Nodes {
		if out.Status.Nodes == nil {
			out.Status.Nodes = wfv1.Nodes{}
		}
		out.Status.Nodes[id] = wfv1.NodeStatus{ID: n.ID, Name: n.Name, Type: n.Type, Phase: n.Phase, SynchronizationStatus: n.SynchronizationStatus}
	}
	return out
}
