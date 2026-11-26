package argojob

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestInPodGoneAttemptNeedsDurableReceipt(t *testing.T) {
	t.Parallel()
	w := &wf.Workflow{Status: wf.WorkflowStatus{Nodes: wf.Nodes{
		"held": {ID: "held", Type: wf.NodeTypePod, Phase: wf.NodeRunning},
		"gone": {ID: "gone", Type: wf.NodeTypePod, Phase: wf.NodeSucceeded},
	}}}
	if err := inPodNativeReceipts(w, nil, "held"); err == nil {
		t.Fatal("gone attempt was carried without a durable receipt")
	}
	rows := []stateport.Node{{NodeID: "gone", PodUID: "collected-pod", Status: stepmodel.StepPassed}}
	if err := inPodNativeReceipts(w, rows, "held"); err != nil {
		t.Fatal("durable carried attempt refused", err)
	}
	n := w.Status.Nodes["gone"]
	n.Phase = wf.NodeRunning
	w.Status.Nodes["gone"] = n
	if err := inPodNativeReceipts(w, rows, "held"); err == nil {
		t.Fatal("another running native attempt was carried")
	}
}
