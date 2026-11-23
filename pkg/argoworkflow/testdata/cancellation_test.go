package controller

import (
	"strings"
	"sync"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOberthShutdownMessageCanBeReplacedByPodAssessment(t *testing.T) {
	ctx := logging.TestContext(t.Context())
	cancel, controller := newController(ctx, defaultServiceAccount)
	defer cancel()
	w := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "shutdown-evidence", Namespace: "default"}, Spec: wfv1.WorkflowSpec{
		Entrypoint: "build", Shutdown: wfv1.ShutdownStrategyStop,
		Templates: []wfv1.Template{{Name: "build", Container: &apiv1.Container{Image: "busybox"}}},
	}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning, Nodes: wfv1.Nodes{}}}
	n := wfv1.NodeStatus{ID: w.Name, Name: w.Name, TemplateName: "build", Type: wfv1.NodeTypePod, Phase: wfv1.NodeRunning}
	w.Status.Nodes[n.ID] = n
	pod := &apiv1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "work", Namespace: "default", Annotations: map[string]string{common.AnnotationKeyNodeID: n.ID}},
		Status: apiv1.PodStatus{Phase: apiv1.PodRunning}}
	op := newWorkflowOperationCtx(ctx, w, controller)
	op.applyExecutionControl(ctx, pod, &sync.RWMutex{})
	stopped := op.wf.Status.Nodes[n.ID]
	if stopped.Phase != wfv1.NodeFailed || stopped.Message != "workflow shutdown with strategy:  Stop" {
		t.Fatal("running node lacks shutdown evidence", stopped)
	}
	pod.Status.Phase = apiv1.PodFailed
	pod.Status.ContainerStatuses = []apiv1.ContainerStatus{{Name: "main", State: apiv1.ContainerState{Terminated: &apiv1.ContainerStateTerminated{ExitCode: 143, Reason: "Error"}}}}
	assessed := op.assessNodeStatus(ctx, pod, &stopped)
	if assessed == nil || assessed.Message == stopped.Message || !strings.Contains(assessed.Message, "143") {
		t.Fatal("pod assessment did not replace the shutdown message", assessed)
	}
	op.wf.Status.Nodes[n.ID] = *assessed
	op.applyExecutionControl(ctx, pod, &sync.RWMutex{})
	if got := op.wf.Status.Nodes[n.ID]; got.Message != assessed.Message {
		t.Fatal("shutdown relabelled an already completed node", got)
	}
}

func TestOberthStopAllowsSuccessfulExitHandlerToFinish(t *testing.T) {
	ctx := logging.TestContext(t.Context())
	cancel, controller := newController(ctx, defaultServiceAccount)
	defer cancel()
	w := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "stop-after-entry", Namespace: "default"}, Spec: wfv1.WorkflowSpec{
		Entrypoint: "build", OnExit: "cleanup",
		Templates: []wfv1.Template{
			{Name: "build", Container: &apiv1.Container{Image: "busybox", Command: []string{"true"}}},
			{Name: "cleanup", Container: &apiv1.Container{Image: "busybox", Command: []string{"true"}}},
		},
	}}
	w = lockRun(ctx, t, controller, w)
	makePodsPhase(ctx, newWorkflowOperationCtx(ctx, w, controller), apiv1.PodSucceeded)
	w = lockRun(ctx, t, controller, w)
	if w.Status.Phase != wfv1.WorkflowRunning || w.Status.Nodes[w.Name].Phase != wfv1.NodeSucceeded {
		t.Fatal("entry did not succeed while the exit handler remained active", w.Status)
	}
	w.Spec.Shutdown = wfv1.ShutdownStrategyStop
	if _, err := controller.wfclientset.ArgoprojV1alpha1().Workflows("default").Update(ctx, w, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	makePodsPhase(ctx, newWorkflowOperationCtx(ctx, w, controller), apiv1.PodSucceeded)
	w = lockRun(ctx, t, controller, w)
	if w.Status.Phase != wfv1.WorkflowSucceeded || w.Status.Nodes[w.Name].Phase != wfv1.NodeSucceeded {
		t.Fatal("Stop replaced a successful entry and exit handler", w.Status)
	}
}

// A queued cancellation must use Terminate: Stop permits an exit handler
// even when the entrypoint has never created a Pod.
func TestOberthQueuedCancellationNeverStartsWork(t *testing.T) {
	for _, mode := range []wfv1.ShutdownStrategy{wfv1.ShutdownStrategyStop, wfv1.ShutdownStrategyTerminate} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := logging.TestContext(t.Context())
			cancel, controller := newController(ctx, defaultServiceAccount)
			defer cancel()
			w := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "cancel-queued", Namespace: "default"}, Spec: wfv1.WorkflowSpec{
				Entrypoint: "build", OnExit: "cleanup", Shutdown: mode,
				Templates: []wfv1.Template{
					{Name: "build", Container: &apiv1.Container{Image: "busybox", Command: []string{"sleep", "600"}}},
					{Name: "cleanup", Container: &apiv1.Container{Image: "busybox", Command: []string{"true"}}},
				},
			}}
			w = lockRun(ctx, t, controller, w)
			pods, err := controller.kubeclientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if mode == wfv1.ShutdownStrategyStop {
				if len(pods.Items) != 1 || !w.Status.Nodes.Any(func(n wfv1.NodeStatus) bool { return n.TemplateName == "cleanup" }) {
					t.Fatal("Stop did not retain its exit handler", w.Status, pods.Items)
				}
			} else if len(pods.Items) != 0 || !w.Status.Fulfilled() {
				t.Fatal("Terminate created work or did not finish", w.Status, pods.Items)
			}
			node := w.Status.Nodes[w.Name]
			if node.Type != wfv1.NodeTypePod || node.Phase != wfv1.NodeFailed || node.Message != "workflow shutdown with strategy: "+string(mode) || node.StartedAt.IsZero() {
				t.Fatal("shutdown before Pod creation changed its native evidence", node)
			}
			if node.Outputs != nil && node.Outputs.ExitCode != nil {
				t.Fatal("unstarted shutdown recorded an exit code", node)
			}
		})
	}
}
