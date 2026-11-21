package argojob_test

import (
	"context"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	"testing"
)

func TestNativePhaseWinsWithoutExitOrAfterZeroExit(t *testing.T) {
	t.Parallel()
	for _, phase := range []wf.NodePhase{wf.NodeFailed, wf.NodeError} {
		for _, podExit := range []bool{false, true} {
			w := nativeWorkflow()
			w.Status.Phase = wf.WorkflowFailed
			n := nativeNode("one", ".build", phase, 1)
			w.Status.Nodes[n.ID] = n
			in := argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}}
			if podExit {
				in.Pods = []*core.Pod{nativePod(w, n, 0)}
			}
			out := project(t, in)
			if out.Steps[0].Status != stepmodel.StepFailed || out.Steps[0].DisplayReason != "step failed" || out.Failure.Class != stepmodel.FailureUnknown {
				t.Fatal(out)
			}
		}
	}
	w := nativeWorkflow()
	w.Status.Phase = ""
	out := project(t, argojob.Projection{Workflow: w})
	if out.Status != stepmodel.RunQueued || out.Reason != "waiting to start" {
		t.Fatal(out)
	}
	w.Status.Phase = wf.WorkflowError
	out = project(t, argojob.Projection{Workflow: w})
	if out.Reason != "run failed" {
		t.Fatal(out)
	}
}
func TestSuspendAndTypedSkippedNodesRemainVisible(t *testing.T) {
	t.Parallel()
	for _, kind := range []wf.NodeType{wf.NodeTypeSuspend, wf.NodeTypeSkipped} {
		w := nativeWorkflow()
		n := nativeNode("one", ".build", wf.NodeSucceeded, 1)
		n.Type = kind
		if kind == wf.NodeTypeSkipped {
			n.Phase = wf.NodeOmitted
		}
		w.Status.Nodes[n.ID] = n
		out := project(t, argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}})
		want := stepmodel.StepPassed
		if kind == wf.NodeTypeSkipped {
			want = stepmodel.StepSkipped
		}
		if out.Steps[0].Status != want || out.Steps[0].AttemptCount != 1 {
			t.Fatal(out)
		}
	}
}
func TestDynamicFailureAndOlderReplayAreMonotone(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	n := nativeNode("one", ".fan(0:x)", wf.NodeFailed, 1)
	w.Status.Nodes[n.ID] = n
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "fan", Dynamic: true}}}
	first := project(t, argojob.Projection{Workflow: w, Plan: plan, Pods: []*core.Pod{nativePod(w, n, 7)}})
	for _, missing := range []bool{false, true} {
		if missing {
			w.Status.Nodes = nil
		} else {
			n.Phase = wf.NodeRunning
			w.Status.Nodes[n.ID] = n
		}
		w.Status.Phase = wf.WorkflowFailed
		w.Spec.Shutdown = wf.ShutdownStrategyStop
		out := project(t, argojob.Projection{Workflow: w, Plan: plan, Previous: first.Steps, PreviousNodes: first.Nodes})
		found := false
		for _, s := range out.Steps {
			if s.Path == "fan(0:x)" {
				found = s.Status == stepmodel.StepFailed
			}
		}
		if !found || out.Nodes[0].Status != stepmodel.StepFailed || out.Status != stepmodel.RunFailed || missing && out.Complete {
			t.Fatal("failure replayed or disappeared", out)
		}
	}
}
func TestRetrySuccessWithNewNativeIdentityClearsFailure(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	n := nativeNode("old", ".build", wf.NodeFailed, 1)
	w.Status.Nodes[n.ID] = n
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}
	first := project(t, argojob.Projection{Workflow: w, Plan: plan, Pods: []*core.Pod{nativePod(w, n, 7)}})
	next := nativeNode("new", ".build", wf.NodeSucceeded, 2)
	w.Status.Phase = wf.WorkflowSucceeded
	w.Status.Nodes = wf.Nodes{next.ID: next}
	out := project(t, argojob.Projection{Workflow: w, Plan: plan, Pods: []*core.Pod{nativePod(w, next, 0)}, Previous: first.Steps, PreviousNodes: first.Nodes})
	if out.Steps[0].Status != stepmodel.StepPassed || out.Failure != nil {
		t.Fatal(out)
	}
}
func TestUnscheduledReasonNeedsTypedCauseAndProjectionCancels(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	n := nativeNode("one", ".build", wf.NodePending, 1)
	w.Status.Nodes[n.ID] = n
	p := nativePod(w, n, 0)
	p.Status.ContainerStatuses = nil
	p.Status.Conditions = []core.PodCondition{{Type: core.PodScheduled, Status: core.ConditionFalse, Reason: "Other"}}
	in := argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Pods: []*core.Pod{p}}
	if out := project(t, in); out.Steps[0].DisplayReason == "waiting for a node" {
		t.Fatal("guessed cause")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, compressed := range []string{"", "invalid"} {
		w.Status.CompressedNodes = compressed
		if _, err := argojob.Project(ctx, in); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
}
func TestRawNodeIdentityAndTerminalStateSurvivePartialReplay(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	first, last := nativeNode("first", ".build(0)", wf.NodeFailed, 1), nativeNode("last", ".build(1)", wf.NodeSucceeded, 2)
	parent := wf.NodeStatus{ID: "retry", Type: wf.NodeTypeRetry, Name: w.Name + ".build", Children: []string{first.ID, last.ID}}
	w.Status.Nodes = wf.Nodes{first.ID: first, last.ID: last, parent.ID: parent}
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}
	before := project(t, argojob.Projection{Workflow: w, Plan: plan, Pods: []*core.Pod{nativePod(w, first, 7), nativePod(w, last, 0)}})
	parent.Children = []string{last.ID}
	last.Phase = wf.NodeRunning
	last.Outputs = nil
	w.Status.Nodes = wf.Nodes{last.ID: last, parent.ID: parent}
	after := project(t, argojob.Projection{Workflow: w, Plan: plan, Previous: before.Steps, PreviousNodes: before.Nodes})
	found := false
	for _, n := range after.Nodes {
		if n.NodeID == last.ID {
			found = n.Attempt == 2 && n.Status == stepmodel.StepPassed
		}
	}
	if !found || after.Complete || after.Steps[0].Status != stepmodel.StepPassed {
		t.Fatal("terminal receipt regressed", after)
	}
}
func TestOffloadedAndDisappearedNodesNeverComplete(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	first, last := nativeNode("first", ".build(0)", wf.NodeFailed, 1), nativeNode("last", ".build(1)", wf.NodeSucceeded, 2)
	retry := wf.NodeStatus{ID: "retry", Type: wf.NodeTypeRetry, Name: w.Name + ".build", Children: []string{first.ID, last.ID}}
	w.Status.Nodes = wf.Nodes{first.ID: first, last.ID: last, retry.ID: retry}
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}
	in := argojob.Projection{Workflow: w, Plan: plan, Pods: []*core.Pod{nativePod(w, first, 7), nativePod(w, last, 0)}}
	before := project(t, in)
	if !before.Complete {
		t.Fatal("fixture incomplete")
	}
	w.Status.OffloadNodeStatusVersion = "remote"
	if project(t, in).Complete {
		t.Fatal("offload claimed complete")
	}
	w.Status.OffloadNodeStatusVersion = ""
	retry.Children = []string{last.ID}
	w.Status.Nodes = wf.Nodes{last.ID: last, retry.ID: retry}
	in.PreviousNodes = before.Nodes
	if project(t, in).Complete {
		t.Fatal("disappeared node claimed complete")
	}
}
func TestNativePassedVerdictClearsToleratedFailure(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowSucceeded
	n := nativeNode("one", ".build", wf.NodeFailed, 1)
	w.Status.Nodes[n.ID] = n
	out := project(t, argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Pods: []*core.Pod{nativePod(w, n, 7)}})
	if out.Status != stepmodel.RunPassed || out.Failure != nil {
		t.Fatal(out)
	}
}
func TestChildBeforeRetryParentConvergesWithoutGuessing(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	n := nativeNode("second", ".build(1)", wf.NodeSucceeded, 2)
	w.Status.Nodes[n.ID] = n
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}
	before := project(t, argojob.Projection{Workflow: w, Plan: plan})
	if before.Complete || before.Steps[0].Status == stepmodel.StepPassed {
		t.Fatal("missing parent guessed")
	}
	w.Status.Nodes["retry"] = wf.NodeStatus{ID: "retry", Name: w.Name + ".build", Type: wf.NodeTypeRetry, Children: []string{"missing", n.ID}}
	after := project(t, argojob.Projection{Workflow: w, Plan: plan, Previous: before.Steps, PreviousNodes: before.Nodes})
	if after.Steps[0].Status != stepmodel.StepPassed {
		t.Fatal("parent did not resolve native path", after)
	}
}
func TestKnownFailureSurvivesStoppedReplayOfSameFailedNode(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	n := nativeNode("one", ".build", wf.NodeFailed, 1)
	w.Status.Nodes[n.ID] = n
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}
	before := project(t, argojob.Projection{Workflow: w, Plan: plan, Pods: []*core.Pod{nativePod(w, n, 7)}})
	w.Status.Phase = wf.WorkflowFailed
	w.Spec.Shutdown = wf.ShutdownStrategyStop
	after := project(t, argojob.Projection{Workflow: w, Plan: plan, Previous: before.Steps, PreviousNodes: before.Nodes})
	if after.Status != stepmodel.RunFailed || after.Steps[0].DisplayReason != "exited with code 7" {
		t.Fatal("stop erased an observed failure", after)
	}
}
