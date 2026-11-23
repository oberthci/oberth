package argojob_test

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestShutdownPreservesSuccessfulNativeRoot(t *testing.T) {
	t.Parallel()
	for _, phase := range []wf.NodePhase{wf.NodeSucceeded, wf.NodeFailed, ""} {
		w := nativeWorkflow()
		w.Status.Phase, w.Spec.Shutdown = wf.WorkflowSucceeded, wf.ShutdownStrategyStop
		if phase != "" {
			n := nativeNode(w.Name, "", phase, 1)
			n.Outputs = &wf.Outputs{ExitCode: new("0")}
			w.Status.Nodes[n.ID] = n
		}
		out := project(t, argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: ""}}}})
		want := stepmodel.RunCancelled
		if phase == wf.NodeSucceeded {
			want = stepmodel.RunPassed
		}
		if out.Status != want || want == stepmodel.RunPassed && out.Failure != nil {
			t.Fatal("shutdown replaced a native success or invented entry success", phase, out)
		}
	}
}
