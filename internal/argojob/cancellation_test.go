package argojob_test

import (
	"strconv"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
)

func TestCancelledExitIsNotAnAuthorFailure(t *testing.T) {
	t.Parallel()
	for _, mode := range []wf.ShutdownStrategy{wf.ShutdownStrategyStop, wf.ShutdownStrategyTerminate} {
		for _, podPresent := range []bool{false, true} {
			t.Run(string(mode)+"/pod="+strconv.FormatBool(podPresent), func(t *testing.T) {
				w := nativeWorkflow()
				n := nativeNode("one", ".build", wf.NodeFailed, 1)
				n.Outputs = &wf.Outputs{ExitCode: new("143")}
				w.Status.Nodes[n.ID] = n
				in := argojob.Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build", Name: "build"}}}, Workflow: w}
				if podPresent {
					in.Pods = []*core.Pod{nativePod(w, n, 143)}
				}
				before := project(t, in)
				in.Previous, in.PreviousNodes = before.Steps, before.Nodes
				w.Spec.Shutdown = mode
				w.Status.Phase = wf.WorkflowFailed
				out := project(t, in)
				if out.Status != stepmodel.RunCancelled || out.Reason != "stopped" || out.Failure.Class != stepmodel.FailureCancelled || out.Failure.Retry.Action != stepmodel.RetryNone || out.Failure.Retry.ReasonCode != "cancelled" {
					t.Fatal("cancel treated as code failure", out, out.Failure)
				}
				if out.Steps[0].DisplayReason != "stopped" || out.Nodes[0].Exit == nil || *out.Nodes[0].Exit != 143 {
					t.Fatal("cancel lost step reason or native exit", out.Steps, out.Nodes)
				}
			})
		}
	}
}

func TestCancelledQueuedRunHasNoStartedSteps(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	w.Spec.Shutdown = wf.ShutdownStrategyTerminate
	w.Status.Phase = wf.WorkflowFailed
	out := project(t, argojob.Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Workflow: w})
	if out.Status != stepmodel.RunCancelled || out.Failure.Class != stepmodel.FailureCancelled || out.Steps[0].Status != stepmodel.StepSkipped || out.Steps[0].StartedAt != nil {
		t.Fatal("queued cancellation became execution", out)
	}
}

func TestCancellationBeforeCreatePostsOnlyTerminatedWorkflow(t *testing.T) {
	t.Parallel()
	p, _, _ := fixture(t)
	runs := memoryAdmission(t, p)
	r, err := runs.Run(t.Context(), p.View().RunID)
	compositionOK(t, err)
	_, err = runs.Cancel(t.Context(), stateport.Action{Actor: "worker"}, stateport.RunCancellation{ID: r.ID, Revision: r.Revision, Terminate: true})
	compositionOK(t, err)
	native := &fake.Workflows{BeforeCreate: func(w *wf.Workflow) {
		if w.Spec.Shutdown != wf.ShutdownStrategyTerminate {
			t.Fatal("cancelled admission posted executable work")
		}
	}}
	e := argojob.New(argojob.Config{Workflows: native, Runs: runs})
	_, err = e.Create(t.Context(), p)
	compositionOK(t, err)
}

func TestCancelDoesNotRewriteFinishedWorkflow(t *testing.T) {
	t.Parallel()
	for _, phase := range []wf.WorkflowPhase{wf.WorkflowSucceeded, wf.WorkflowFailed} {
		e, native, binding := submitted(t)
		native.Object.Status.Phase = phase
		if err := e.Terminate(t.Context(), binding); err != nil || native.Patches != 0 {
			t.Fatal("late cancel rewrote a finished Workflow", phase, native.Patches, err)
		}
	}
}
