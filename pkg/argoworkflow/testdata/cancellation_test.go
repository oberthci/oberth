package controller

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
		})
	}
}
