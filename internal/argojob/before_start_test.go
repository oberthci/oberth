package argojob_test

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	"slices"
	"strings"
	"testing"
)

func TestEndedSourceFailureBeforeOwnContainerStarts(t *testing.T) {
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowError
	n := nativeNode("one", ".build", wf.NodeError, 1)
	n.Message = "source access was forbidden by the controller"
	w.Status.Nodes[n.ID] = n
	p := nativePod(w, n, 0)
	p.Status.ContainerStatuses[0].State = core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "PodInitializing"}}
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build", Name: "build"}}}
	before := project(t, argojob.Projection{Plan: plan, Workflow: w, Pods: []*core.Pod{p}})
	p.Spec.InitContainers = []core.Container{{Name: "init-artifact-oberth"}}
	p.Status.InitContainerStatuses = []core.ContainerStatus{{Name: "init-artifact-oberth", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 64, Message: n.Message}}}}
	out := project(t, argojob.Projection{Plan: plan, Workflow: w, Pods: []*core.Pod{p}, Previous: before.Steps, PreviousNodes: before.Nodes})
	assertSourceFailureBeforeStart(t, out, n.Message)
	replay := project(t, argojob.Projection{Plan: plan, Workflow: w, Pods: []*core.Pod{p}, Previous: out.Steps, PreviousNodes: out.Nodes})
	if replay.Reason != out.Reason || replay.Failure.Class != out.Failure.Class || *replay.Nodes[0].Exit != 64 {
		t.Fatal("replay lost failure", replay)
	}
}

func assertSourceFailureBeforeStart(t *testing.T, out argojob.Observation, message string) {
	t.Helper()
	if out.Status != stepmodel.RunFailed || out.Reason != "failed before starting" || out.Failure == nil || out.Failure.Class != stepmodel.FailureAuthor || out.Failure.Name != "build" || out.Failure.Retry.Action != stepmodel.RetryNone {
		t.Fatalf("unhelpful failure: %+v %+v", out, out.Failure)
	}
	step := out.Steps[0]
	if !strings.Contains(step.Detail, message) || !strings.Contains(step.Detail, "Exit code: 64") || !slices.ContainsFunc(step.Logs, func(hint stepmodel.LogSelector) bool {
		return hint.NodeID == "one" && hint.Container == "init-artifact-oberth"
	}) {
		t.Fatal("lost init diagnosis", step)
	}
	assertFailedInitExit(t, out)
}

func TestEndedSourceFailureWithServerReceiptGrantsRerun(t *testing.T) {
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowError
	n := nativeNode("one", ".build", wf.NodeError, 1)
	n.Message = "source service unavailable"
	w.Status.Nodes[n.ID] = n
	p := nativePod(w, n, 0)
	p.Status.ContainerStatuses[0].State = core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "PodInitializing"}}
	p.Status.InitContainerStatuses = []core.ContainerStatus{{Name: "init-artifact-oberth", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 64, Message: n.Message, FinishedAt: n.FinishedAt}}}}
	r := runlog.InfrastructureReceipt{RunID: "run", WorkflowUID: string(w.UID), NodeID: n.ID, Step: "build", PodUID: string(p.UID), Kind: "source", StartedAt: n.StartedAt.Time, FinishedAt: n.FinishedAt.Time}
	out := project(t, argojob.Projection{RunID: "run", Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build", Name: "build"}}}, Workflow: w, Pods: []*core.Pod{p}, Infrastructure: []runlog.InfrastructureReceipt{r}})
	if out.Failure == nil || out.Failure.Class != stepmodel.FailureInfrastructure || out.Failure.Retry.Action != stepmodel.RetryRerun || out.Reason != "source delivery failed" {
		t.Fatal("server source receipt lost its diagnosis", out.Failure)
	}
	assertFailedInitExit(t, out)
}

func assertFailedInitExit(t *testing.T, out argojob.Observation) {
	t.Helper()
	if len(out.Nodes) != 1 || out.Nodes[0].Exit == nil || *out.Nodes[0].Exit != 64 || !out.Complete {
		t.Fatal("lost actual exit", out.Nodes, out.Complete)
	}
}
