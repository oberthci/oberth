package argojob_test

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

func TestUnstartedDynamicAtTerminalIsNotReached(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowFailed
	out := project(t, argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "fan", Dynamic: true}}}})
	if len(out.Steps) != 1 || out.Steps[0].Status != stepmodel.StepSkipped || out.Steps[0].DisplayReason != "not reached" || out.Complete {
		t.Fatal(out)
	}
}
func TestPriorExpandedPathSurvivesWithoutAnyCurrentNodes(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowFailed
	old := stepmodel.RunStep{Path: "fan(0:a)", Status: stepmodel.StepFailed, AttemptCount: 1, Attempts: []stepmodel.StepAttempt{{NodeID: "old", PodName: "pod", Status: stepmodel.StepFailed}}, DisplayReason: "exited with code 7"}
	out := project(t, argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "fan", Dynamic: true}}}, Previous: []stepmodel.RunStep{old}})
	if len(out.Steps) != 1 || out.Steps[0].Path != old.Path || out.Steps[0].DisplayReason != old.DisplayReason || out.Steps[0].AttemptCount != 1 {
		t.Fatal(out.Steps)
	}
}
func TestStoredExpansionOutsideCurrentPlanRowsRemainsVisible(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	old := stepmodel.RunStep{Path: "retained(0:a)", Status: stepmodel.StepFailed, AttemptCount: 1, Attempts: []stepmodel.StepAttempt{{NodeID: "old", Status: stepmodel.StepFailed}}}
	out := project(t, argojob.Projection{Workflow: w, Previous: []stepmodel.RunStep{old}})
	if len(out.Steps) != 1 || out.Steps[0].Path != old.Path || out.Steps[0].AttemptCount != 1 {
		t.Fatal(out.Steps)
	}
}
func TestStepsGroupUnderRetryKeepsLogicalGroupPath(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	parent := wf.NodeStatus{ID: "retry", Name: w.Name + ".group", Type: wf.NodeTypeRetry, Children: []string{"group"}}
	group := wf.NodeStatus{ID: "group", Name: w.Name + ".group(0)", Type: wf.NodeTypeSteps}
	child := nativeNode("child", ".group(0)[0].build", wf.NodeSucceeded, 1)
	w.Status.Nodes = wf.Nodes{parent.ID: parent, group.ID: group, child.ID: child}
	out := project(t, argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "group[0].build"}}}})
	if len(out.Nodes) != 1 || out.Nodes[0].Path != "group[0].build" || out.Steps[0].AttemptCount != 1 {
		t.Fatal(out)
	}
}
