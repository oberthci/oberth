package argojob_test

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestShutdownBeforePodCreationReadsStopped(t *testing.T) {
	t.Parallel()
	for _, mode := range []wf.ShutdownStrategy{wf.ShutdownStrategyTerminate, wf.ShutdownStrategyStop} {
		t.Run(string(mode), func(t *testing.T) {
			w := nativeWorkflow()
			w.Spec.Entrypoint, w.Spec.Shutdown, w.Status.Phase = "build", mode, wf.WorkflowFailed
			// The pinned controller creates a failed Pod node, with native times
			// but no Pod or exit code, when shutdown refuses Pod creation.
			n := nativeNode(w.Name, "", wf.NodeFailed, 1)
			n.Message = "workflow shutdown with strategy: " + string(mode)
			w.Status.Nodes[n.ID] = n
			out := project(t, argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "", Name: "build"}}}})
			if out.Status != stepmodel.RunCancelled || out.Failure != nil || len(out.Steps) != 1 {
				t.Fatal("unstarted shutdown lost its cancelled verdict", out)
			}
			step := out.Steps[0]
			if step.Status != stepmodel.StepFailed || step.DisplayReason != "stopped" || step.FailureClass != stepmodel.FailureCancelled {
				t.Fatal("shutdown before Pod creation looks like a step failure", step)
			}
			checkUnstartedShutdownEvidence(t, out)
		})
	}
}

func checkUnstartedShutdownEvidence(t *testing.T, out argojob.Observation) {
	t.Helper()
	step := out.Steps[0]
	if step.StartedAt == nil || len(step.Logs) != 0 || len(out.Nodes) != 1 || out.Nodes[0].Exit != nil || out.Nodes[0].PodName != "" {
		t.Fatal("unstarted shutdown changed native evidence", out)
	}
}
