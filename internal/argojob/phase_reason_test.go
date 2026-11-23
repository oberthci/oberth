package argojob_test

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
)

func TestStepReasonsFollowNodePhaseWhilePodFinishesFirst(t *testing.T) {
	t.Parallel()
	for _, fromPod := range []bool{true, false} {
		t.Run(map[bool]string{true: "pod", false: "node-exit"}[fromPod], func(t *testing.T) {
			replayPhaseReasons(t, fromPod)
		})
	}
}

type phaseReasonPoll struct {
	phase  wf.NodePhase
	exited bool
	status stepmodel.StepStatus
	reason string
}

func replayPhaseReasons(t *testing.T, fromPod bool) {
	t.Helper()
	in := argojob.Projection{Workflow: nativeWorkflow(), Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{
		{Path: "prepare"}, {Path: "test-a"}, {Path: "test-b"},
	}}}
	// Replay prepare, then the two fan-out steps: container termination is
	// visible for several polls before Argo updates each node's phase.
	for _, paths := range [][]string{{"prepare"}, {"test-a", "test-b"}} {
		for _, poll := range []phaseReasonPoll{
			{wf.NodePending, false, stepmodel.StepQueued, "starting containers"},
			{wf.NodePending, true, stepmodel.StepQueued, "waiting to start"},
			{wf.NodeRunning, true, stepmodel.StepRunning, "running"},
			{wf.NodeRunning, true, stepmodel.StepRunning, "running"},
			{wf.NodeSucceeded, true, stepmodel.StepPassed, "passed"},
		} {
			setPhaseReasonPoll(&in, paths, poll, fromPod)
			out := project(t, in)
			assertPhaseReasonPoll(t, out, paths, poll)
			in.Previous, in.PreviousNodes, in.Pods = out.Steps, out.Nodes, nil
		}
	}
}

func setPhaseReasonPoll(in *argojob.Projection, paths []string, poll phaseReasonPoll, fromPod bool) {
	for _, path := range paths {
		n := nativeNode(path, "."+path, poll.phase, 1)
		p := nativePod(in.Workflow, n, 0)
		if !poll.exited {
			p.Status.ContainerStatuses[0].State = core.ContainerState{Running: &core.ContainerStateRunning{}}
		} else if !fromPod {
			exit := "0"
			n.Outputs = &wf.Outputs{ExitCode: &exit}
			p.Status.ContainerStatuses = nil
		}
		in.Workflow.Status.Nodes[n.ID] = n
		in.Pods = append(in.Pods, p)
	}
}

func assertPhaseReasonPoll(t *testing.T, out argojob.Observation, paths []string, poll phaseReasonPoll) {
	t.Helper()
	steps := map[string]stepmodel.RunStep{}
	for _, step := range out.Steps {
		steps[step.Path] = step
	}
	for _, path := range paths {
		step := steps[path]
		if step.Status != poll.status || step.DisplayReason != poll.reason {
			t.Fatalf("%s at %s (exited %v): %s / %q", path, poll.phase, poll.exited, step.Status, step.DisplayReason)
		}
	}
}

func TestTerminalStepDiscardsStalePodWaitAndLockReason(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	n := nativeNode("one", ".build", wf.NodeSucceeded, 1)
	n.SynchronizationStatus = &wf.NodeSynchronizationStatus{Waiting: "ci/Mutex/forge/team/app/publish"}
	w.Status.Nodes[n.ID] = n
	p := nativePod(w, n, 0)
	p.Status.ContainerStatuses[0].State = core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "ContainerCreating"}}
	out := project(t, argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Pods: []*core.Pod{p}})
	if step := out.Steps[0]; step.Status != stepmodel.StepPassed || step.DisplayReason != "passed" {
		t.Fatal("stale wait on a passed step", step)
	}
}
