package controller

import (
	"context"
	"encoding/json"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	"os"
	"path/filepath"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
)

// The pinned controller persists nodes and completion evidence from native task results.
func TestOberthInstallerStatus(t *testing.T) {
	t.Setenv("INFORMER_WRITE_BACK", "true")
	ctx := logging.TestContext(t.Context())
	wf := installerStatusWorkflow()
	// Exercise the native compatibility path for workflows with the older completion map.
	wf.Status.TaskResultsCompletionStatus = map[string]bool{wf.Name: true}
	cancel, controller := newController(ctx, wf, defaultServiceAccount, func(c *WorkflowController) {
		c.Config.InstanceID = "frontdoor"
		c.eventRecorderManager = &testEventRecorderManager{eventRecorder: record.NewFakeRecorder(100)}
	})
	defer cancel()
	woc := newWorkflowOperationCtx(ctx, wf, controller)
	woc.operate(ctx)
	makePodsPhase(ctx, woc, corev1.PodRunning, pendingInstallerOutputs(ctx))
	// Put the native client's actual task result in its informer without waiting on a watch.
	results, err := controller.wfclientset.ArgoprojV1alpha1().WorkflowTaskResults(wf.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(results.Items) != 1 {
		t.Fatal("task result not written", err, results)
	}
	if err := controller.taskResultInformer.GetStore().Add(&results.Items[0]); err != nil {
		t.Fatal(err)
	}
	woc.operate(ctx)
	actual, err := controller.wfclientset.ArgoprojV1alpha1().Workflows(wf.Namespace).Get(ctx, wf.Name, metav1.GetOptions{})
	if err != nil || len(actual.Status.Nodes) == 0 || len(actual.Status.TaskResultsCompletionStatus) == 0 || actual.Status.TaskResultsCompletionStatus[wf.Name] {
		t.Fatal("controller status not persisted", actual, err)
	}
	normalizeInstallerStatus(actual)
	raw, err := json.MarshalIndent(actual, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("OBERTH_CONTROLLER_CASES"), "installer-status.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func normalizeInstallerStatus(wf *wfv1.Workflow) {
	wf.ResourceVersion = ""
	wf.Status.StartedAt, wf.Status.FinishedAt = metav1.Time{}, metav1.Time{}
	wf.Status.EstimatedDuration = 0
	for key := range wf.Status.ResourcesDuration {
		wf.Status.ResourcesDuration[key] = 0
	}
	for id, node := range wf.Status.Nodes {
		node.StartedAt, node.FinishedAt = metav1.Time{}, metav1.Time{}
		node.EstimatedDuration = 0
		for key := range node.ResourcesDuration {
			node.ResourcesDuration[key] = 0
		}
		wf.Status.Nodes[id] = node
	}
}

func pendingInstallerOutputs(ctx context.Context) with {
	return func(pod *corev1.Pod, woc *wfOperationCtx) {
		result := &wfv1.WorkflowTaskResult{
			ObjectMeta: metav1.ObjectMeta{Name: woc.nodeID(pod), Labels: map[string]string{
				common.LabelKeyWorkflow:               woc.wf.Name,
				common.LabelKeyReportOutputsCompleted: "false",
				common.LabelKeyControllerInstanceID:   "frontdoor",
			}},
			NodeResult: wfv1.NodeResult{Outputs: &wfv1.Outputs{Parameters: []wfv1.Parameter{{Name: "value", Value: wfv1.AnyStringPtr("built")}}}},
		}
		if _, err := woc.controller.wfclientset.ArgoprojV1alpha1().WorkflowTaskResults(woc.wf.Namespace).Create(ctx, result, metav1.CreateOptions{}); err != nil {
			panic(err)
		}
	}
}

func installerStatusWorkflow() *wfv1.Workflow {
	return wfv1.MustUnmarshalWorkflow(`
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  name: installer-status
  namespace: default
  labels:
    workflows.argoproj.io/controller-instanceid: frontdoor
spec:
  entrypoint: main
  templates:
  - name: main
    container:
      image: image
      command: ["true"]
`)
}
