package argojob

import (
	"context"
	"errors"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// budgetTestWorkflow returns a terminal workflow with two burns, each
// containing one step pod. The steps have distinct start times so
// StepResults produces a deterministic order: build/compile then test/unit.
// Node IDs and DisplayNames follow the same conventions as nestedWorkflow().
func budgetTestWorkflow() *wfv1.Workflow {
	started1 := metav1.NewTime(time.Unix(1000, 0).UTC())
	finished1 := metav1.NewTime(time.Unix(1005, 0).UTC())
	started2 := metav1.NewTime(time.Unix(1006, 0).UTC())
	finished2 := metav1.NewTime(time.Unix(1010, 0).UTC())
	workflow := &wfv1.Workflow{}
	workflow.Name = "wf"
	workflow.Namespace = testNamespace
	workflow.Annotations = map[string]string{
		runIDAnnotation:    "run-budget",
		identityAnnotation: "digest",
	}
	workflow.Spec = wfv1.WorkflowSpec{
		Entrypoint: "ci",
		Templates: []wfv1.Template{
			{Name: "ci", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "build", Template: "build"},
				{Name: "test", Template: "test"},
			}}},
			{Name: "build", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "compile", Template: "compile"},
			}}},
			{Name: "test", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "unit", Template: "unit"},
			}}},
			{Name: "compile", Container: &corev1.Container{Image: "golang:1.26"}},
			{Name: "unit", Container: &corev1.Container{Image: "golang:1.26"}},
		},
	}
	workflow.Status = wfv1.WorkflowStatus{
		Phase: wfv1.WorkflowSucceeded,
		Nodes: wfv1.Nodes{
			"root": {ID: "root", Name: "wf", DisplayName: "wf",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded},
			"build-burn": {ID: "build-burn", Name: "wf.build", DisplayName: "build",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded, BoundaryID: "root"},
			"test-burn": {ID: "test-burn", Name: "wf.test", DisplayName: "test",
				Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded, BoundaryID: "root"},
			"compile-pod": {ID: "compile-pod", Name: "wf.build.compile", DisplayName: "compile",
				TemplateName: "compile", Type: wfv1.NodeTypePod, Phase: wfv1.NodeSucceeded,
				BoundaryID: "build-burn", StartedAt: started1, FinishedAt: finished1},
			"unit-pod": {ID: "unit-pod", Name: "wf.test.unit", DisplayName: "unit",
				TemplateName: "unit", Type: wfv1.NodeTypePod, Phase: wfv1.NodeSucceeded,
				BoundaryID: "test-burn", StartedAt: started2, FinishedAt: finished2},
		},
	}
	return workflow
}

type staticWorkflowClient struct {
	workflow *wfv1.Workflow
}

func (client *staticWorkflowClient) Create(_ context.Context, _ *wfv1.Workflow, _ metav1.CreateOptions) (*wfv1.Workflow, error) {
	return nil, errors.New("staticWorkflowClient: Create not expected")
}

func (client *staticWorkflowClient) Get(_ context.Context, _ string, _ metav1.GetOptions) (*wfv1.Workflow, error) {
	return client.workflow.DeepCopy(), nil
}

func (client *staticWorkflowClient) Delete(_ context.Context, _ string, _ metav1.DeleteOptions) error {
	return errors.New("staticWorkflowClient: Delete not expected")
}
