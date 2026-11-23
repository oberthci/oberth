package argojob_test

import (
	"reflect"
	"strconv"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
)

func TestCancellationPreservesEarlierFailureBesideStoppedStep(t *testing.T) {
	t.Parallel()
	for _, cause := range []string{"7", "143", "OOMKilled", "DeadlineExceeded", "NodeShutdown"} {
		t.Run(cause, func(t *testing.T) {
			in, before := priorFailureAndRunningSibling(t, cause)
			want := before.Steps[0]
			for _, podsRemain := range []bool{true, false} {
				if !podsRemain {
					in.Pods = nil
				}
				after := project(t, in)
				old, stopped := after.Steps[0], after.Steps[1]
				if old.DisplayReason != want.DisplayReason || old.FailureClass != want.FailureClass || before.Nodes[0].Exit != nil && !reflect.DeepEqual(after.Nodes[0].Exit, before.Nodes[0].Exit) {
					t.Fatal("cancel relabelled an earlier failure", want, old, after.Nodes)
				}
				if after.Status != stepmodel.RunCancelled || stopped.DisplayReason != "stopped" || stopped.FailureClass != stepmodel.FailureCancelled || *after.Nodes[1].Exit != 143 {
					t.Fatal("shutdown did not own the stopped sibling", after)
				}
				in.Previous, in.PreviousNodes = after.Steps, after.Nodes
			}
		})
	}
}

func priorFailureAndRunningSibling(t *testing.T, cause string) (argojob.Projection, argojob.Observation) {
	t.Helper()
	w := nativeWorkflow()
	old := nativeNode("a", ".failed", wf.NodeFailed, 1)
	running := nativeNode("b", ".active", wf.NodeRunning, 20)
	exit, err := strconv.Atoi(cause)
	if err != nil {
		exit = 137
	}
	old.Outputs = &wf.Outputs{ExitCode: new(strconv.Itoa(exit))}
	pod := nativePod(w, old, int32(exit))
	if cause == "OOMKilled" {
		pod.Status.ContainerStatuses[0].State.Terminated.Reason = cause
	} else if err != nil {
		pod.Status.Reason = cause
	}
	w.Status.Nodes = wf.Nodes{old.ID: old, running.ID: running}
	in := argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "failed"}, {Path: "active"}}}, Pods: []*core.Pod{pod}}
	before := project(t, in)
	w.Spec.Shutdown, w.Status.Phase = wf.ShutdownStrategyStop, wf.WorkflowFailed
	running.Phase, running.Outputs = wf.NodeFailed, &wf.Outputs{ExitCode: new("143")}
	w.Status.Nodes[running.ID] = running
	in.Previous, in.PreviousNodes = before.Steps, before.Nodes
	in.Pods = append(in.Pods, nativePod(w, running, 143))
	return in, before
}

func TestShutdownNeedsNodeEvidenceWithoutPriorObservation(t *testing.T) {
	t.Parallel()
	for _, message := range []string{"", "workflow shutdown with strategy:  Stop", "Stopped with strategy 'Stop'"} {
		w := nativeWorkflow()
		w.Spec.Shutdown, w.Status.Phase = wf.ShutdownStrategyStop, wf.WorkflowFailed
		n := nativeNode("a", ".build", wf.NodeFailed, 1)
		n.Message, n.Outputs = message, &wf.Outputs{ExitCode: new("143")}
		w.Status.Nodes[n.ID] = n
		out := project(t, argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}})
		want := stepmodel.FailureAuthor
		if message != "" {
			want = stepmodel.FailureCancelled
		}
		if out.Steps[0].FailureClass != want {
			t.Fatal("workflow shutdown alone classified a node", message, out.Steps)
		}
	}
}

func TestStopDoesNotRelabelExitHandlerFailure(t *testing.T) {
	t.Parallel()
	in, _ := priorFailureAndRunningSibling(t, "7")
	n := in.Workflow.Status.Nodes["b"]
	n.DisplayName = "run-0001.onExit"
	in.Workflow.Status.Nodes[n.ID] = n
	got := project(t, in).Steps[1]
	if got.DisplayReason != "exited with code 143" || got.FailureClass != stepmodel.FailureAuthor {
		t.Fatal("Stop relabelled its allowed exit handler", got)
	}
}

func TestLaterTypedDiagnosisRefinesNativeExit(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	n := nativeNode("a", ".build", wf.NodeFailed, 1)
	n.Outputs = &wf.Outputs{ExitCode: new("137")}
	w.Status.Nodes[n.ID] = n
	in := argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}}
	before := project(t, in)
	in.Previous, in.PreviousNodes = before.Steps, before.Nodes
	pod := nativePod(w, n, 137)
	pod.Status.ContainerStatuses[0].State.Terminated.Reason = "OOMKilled"
	in.Pods = []*core.Pod{pod}
	if got := project(t, in).Steps[0]; got.DisplayReason != "out of memory" || got.FailureClass != stepmodel.FailureAuthor {
		t.Fatal("previous exit hid a later typed diagnosis", got)
	}
}
