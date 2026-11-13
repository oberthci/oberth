package argojob

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/workflow/packer"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/model"
)

func TestCompressedNativeSnapshotKeeps2000NodesAndContainerBindings(t *testing.T) {
	workflow, pod := nativeContainerFixture()
	for i := 2; i < 2000; i++ {
		id := fmt.Sprintf("node-%04d", i)
		workflow.Status.Nodes[id] = wfv1.NodeStatus{ID: id, DisplayName: "same", TemplateName: "leaf", Type: wfv1.NodeTypePod, Phase: wfv1.NodePending, Message: strings.Repeat("native detail ", 100)}
	}
	if err := packer.CompressWorkflowIfNeeded(t.Context(), workflow); err != nil {
		t.Fatal(err)
	}
	if workflow.Status.CompressedNodes == "" || len(workflow.Status.Nodes) != 0 {
		t.Fatal("fixture did not exercise native compression")
	}
	before := workflow.DeepCopy()
	rows := NodeStepResults(workflow, []*corev1.Pod{pod})
	bindings := StepContainers(workflow, []*corev1.Pod{pod})
	if len(rows) != 2000 || len(bindings) != 3 {
		t.Fatal("compressed native nodes lost", len(rows), bindings)
	}
	if !reflect.DeepEqual(workflow, before) {
		t.Fatal("shared Workflow cache object changed")
	}
}

type snapshotStepWriter struct{ writes int }

func (w *snapshotStepWriter) PutStepResults(context.Context, string, []model.StepResult) error {
	w.writes++
	return nil
}

func TestRecordNodeStepsDoesNotClearRowsOnUnavailableNativeStatus(t *testing.T) {
	for _, kind := range []string{"compressed", "offloaded"} {
		t.Run(kind, func(t *testing.T) {
			writer := &snapshotStepWriter{}
			controller := (&Controller{}).WithStepResultWriter(writer)
			workflow, _ := nativeContainerFixture()
			if err := controller.RecordNodeSteps(t.Context(), workflow, nil); err != nil {
				t.Fatal(err)
			}
			workflow.Status.Nodes = nil
			if kind == "compressed" {
				workflow.Status.CompressedNodes = "invalid"
			} else {
				workflow.Status.OffloadNodeStatusVersion = "native-version"
			}
			if err := controller.RecordNodeSteps(t.Context(), workflow, nil); err == nil {
				t.Fatal("missing native status accepted as an empty snapshot")
			}
			if writer.writes != 1 {
				t.Fatal("existing rows were rewritten on a missing observation", writer.writes)
			}
		})
	}
}
