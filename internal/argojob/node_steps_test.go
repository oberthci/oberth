package argojob

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
)

func TestNodeStepResultsRetainsFanoutRetriesAndSkippedNodes(t *testing.T) {
	workflow, pod := nativeContainerFixture()
	workflow.Annotations = map[string]string{runIDAnnotation: "run-id"}
	workflow.Labels = map[string]string{"oberth.ci/engine": "argo"}
	workflow.Status.Nodes = wfv1.Nodes{}
	at := metav1.NewTime(time.Unix(1000, 0))
	for i, phase := range []wfv1.NodePhase{wfv1.NodeFailed, wfv1.NodeSucceeded, wfv1.NodeSkipped, wfv1.NodeOmitted} {
		id := fmt.Sprintf("node-%d", i)
		workflow.Status.Nodes[id] = wfv1.NodeStatus{ID: id, Name: fmt.Sprintf("native-run.branch(%d).check", i), DisplayName: "check", TemplateName: "shared-leaf", Type: wfv1.NodeTypePod, Phase: phase, Message: "native message\nwith details", StartedAt: at}
	}
	workflow.Status.Nodes["retry"] = wfv1.NodeStatus{ID: "retry", Name: "native-run.retry", Type: wfv1.NodeTypeRetry, Phase: wfv1.NodeSucceeded, Children: []string{"node-0", "node-1"}}
	pod.Annotations["workflows.argoproj.io/node-id"] = "node-1"
	pod.Annotations["workflows.argoproj.io/node-name"] = workflow.Status.Nodes["node-1"].Name
	before := workflow.DeepCopy()
	got := NodeStepResults(workflow, []*corev1.Pod{pod})
	if len(got) != 5 || !reflect.DeepEqual(before, workflow) {
		t.Fatalf("node rows collapsed or input mutated: %+v", got)
	}
	byID := map[string]model.StepResult{}
	for _, row := range got {
		byID[row.NodeID] = row
	}
	for i, status := range []model.StepStatus{model.StepFailed, model.StepPassed, model.StepSkipped, model.StepSkipped} {
		row := byID[fmt.Sprintf("node-%d", i)]
		if row.Status != status || row.Step != "check" || row.DisplayName != "check" || row.Template != "shared-leaf" || row.Message != "native message\nwith details" || row.FinishedAt != nil || row.StartedAt == nil || !row.StartedAt.Equal(at.Time) {
			t.Fatalf("native node changed: %+v", row)
		}
	}
	if byID["node-1"].PodName != pod.Name || byID["node-0"].PodName != "" {
		t.Fatal("Pod name was guessed or lost", byID)
	}
}

func TestNodeStepResultsHasNoNodeCap(t *testing.T) {
	workflow := &wfv1.Workflow{}
	workflow.Status.Nodes = wfv1.Nodes{}
	for i := range 2000 {
		id := fmt.Sprintf("node-%04d", i)
		workflow.Status.Nodes[id] = wfv1.NodeStatus{ID: id, DisplayName: "same", TemplateName: "leaf", Type: wfv1.NodeTypePod, Phase: wfv1.NodePending}
	}
	rows := NodeStepResults(workflow, nil)
	if len(rows) != 2000 {
		t.Fatalf("node count = %d", len(rows))
	}
	for i, row := range rows {
		if row.Ordinal != i || row.NodeID != fmt.Sprintf("node-%04d", i) || row.Status != model.StepQueued || row.StartedAt != nil || row.FinishedAt != nil {
			t.Fatalf("native row changed: %+v", row)
		}
	}
}

func TestNodeStepResultsBindsOnlyOwnLocalTemplateScope(t *testing.T) {
	for _, test := range []struct {
		scope string
		want  bool
	}{
		{"", true}, {"local/native-run", true}, {"local/another-workflow", false}, {"namespaced/shared", false}, {"local", false},
	} {
		workflow, _ := nativeContainerFixture()
		node := workflow.Status.Nodes["pod-node"]
		node.TemplateScope = test.scope
		workflow.Status.Nodes = wfv1.Nodes{"pod-node": node}
		rows := NodeStepResults(workflow, nil)
		if len(rows) != 1 || rows[0].LocalTemplate != test.want {
			t.Fatalf("scope %q: %+v", test.scope, rows)
		}
	}
}
