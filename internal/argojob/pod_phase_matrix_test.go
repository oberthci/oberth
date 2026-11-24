package argojob_test

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
)

func TestPodDiagnosisFollowsNodePhase(t *testing.T) {
	t.Parallel()
	for _, phase := range []wf.NodePhase{"", wf.NodePending, wf.NodeRunning, wf.NodeSucceeded,
		wf.NodeFailed, wf.NodeError, wf.NodeSkipped, wf.NodeOmitted} {
		for _, state := range []string{"absent", "creating", "image", "running", "exit", "oom",
			"init", "evicted", "disrupted", "deadline", "shutdown", "unschedulable", "config"} {
			t.Run(string(phase)+"/"+state, func(t *testing.T) {
				in := podPhaseInput(phase, state)
				out := project(t, in)
				assertPodPhase(t, out.Steps[0], phase, state)
				assertPodEvidence(t, out, phase, state)
			})
		}
	}
}

func assertPodEvidence(t *testing.T, out argojob.Observation, phase wf.NodePhase, state string) {
	t.Helper()
	if phase != wf.NodePending && phase != wf.NodeFailed && phase != wf.NodeError {
		return
	}
	switch state {
	case "evicted", "disrupted", "deadline", "shutdown":
		if out.Steps[0].Detail != "pod evidence" {
			t.Fatal("Pod diagnosis lost its detail", out.Steps)
		}
	case "exit", "oom", "init":
		if out.Nodes[0].Exit == nil || *out.Nodes[0].Exit != 7 {
			t.Fatal("container termination evidence was lost", out.Nodes)
		}
	}
}

func TestPendingPodFailureDoesNotRegressWaitingReason(t *testing.T) {
	t.Parallel()
	for _, first := range []string{"creating", "image", "running"} {
		for _, ended := range []string{"exit", "oom", "init", "evicted", "disrupted", "deadline", "shutdown"} {
			t.Run(first+"/"+ended, func(t *testing.T) {
				in := podPhaseInput(wf.NodePending, first)
				a := project(t, in)
				in = podPhaseInput(wf.NodePending, ended)
				in.Previous, in.PreviousNodes = a.Steps, a.Nodes
				b := project(t, in)
				if b.Steps[0].DisplayReason != "starting containers" || b.Reason != "starting containers" {
					t.Fatal("ended Pod regressed the queued reason", a, b)
				}
				in = podPhaseInput(wf.NodeFailed, ended)
				in.Previous, in.PreviousNodes = b.Steps, b.Nodes
				c := project(t, in)
				assertPodPhase(t, c.Steps[0], wf.NodeFailed, ended)
				in = podPhaseInput(wf.NodePending, ended)
				in.Previous, in.PreviousNodes = c.Steps, c.Nodes
				d := project(t, in)
				assertPodPhase(t, d.Steps[0], wf.NodeFailed, ended)
			})
		}
	}
}

func podPhaseInput(phase wf.NodePhase, state string) argojob.Projection {
	w := nativeWorkflow()
	n := nativeNode("one", ".build", phase, 1)
	w.Status.Nodes[n.ID] = n
	in := argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{
		Entries: []admissioncontract.PlanEntry{{Path: "build"}},
	}}
	if state == "absent" {
		return in
	}
	p := nativePod(w, n, 7)
	p.Status.Message = "pod evidence"
	c := &p.Status.ContainerStatuses[0]
	switch state {
	case "creating", "image", "config":
		reason := map[string]string{"creating": "ContainerCreating", "image": "ImagePullBackOff", "config": "CreateContainerConfigError"}[state]
		c.State = core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: reason}}
	case "running":
		c.State = core.ContainerState{Running: &core.ContainerStateRunning{}}
	case "oom":
		c.State.Terminated.Reason = "OOMKilled"
	case "init":
		p.Status.InitContainerStatuses = []core.ContainerStatus{{Name: "authored-init", State: c.State}}
	case "evicted", "deadline", "shutdown":
		p.Status.Reason = map[string]string{"evicted": "Evicted", "deadline": "DeadlineExceeded", "shutdown": "NodeShutdown"}[state]
		c.State = core.ContainerState{}
	case "disrupted":
		c.State = core.ContainerState{}
		p.Status.Conditions = []core.PodCondition{{Type: core.DisruptionTarget, Status: core.ConditionTrue, Message: "pod evidence"}}
	case "unschedulable":
		p.Status.ContainerStatuses = nil
		p.Status.Conditions = []core.PodCondition{{Type: core.PodScheduled, Status: core.ConditionFalse, Reason: "Unschedulable"}}
	}
	in.Pods = []*core.Pod{p}
	return in
}

func assertPodPhase(t *testing.T, step stepmodel.RunStep, phase wf.NodePhase, state string) {
	t.Helper()
	status := map[wf.NodePhase]stepmodel.StepStatus{"": stepmodel.StepUnknown,
		wf.NodePending: stepmodel.StepQueued, wf.NodeRunning: stepmodel.StepRunning,
		wf.NodeSucceeded: stepmodel.StepPassed, wf.NodeFailed: stepmodel.StepFailed,
		wf.NodeError: stepmodel.StepFailed, wf.NodeSkipped: stepmodel.StepSkipped, wf.NodeOmitted: stepmodel.StepSkipped}[phase]
	if step.Status != status {
		t.Fatal("status disagrees with native phase", step)
	}
	reason := map[wf.NodePhase]string{"": "unknown", wf.NodeSucceeded: "passed",
		wf.NodeSkipped: "skipped by condition", wf.NodeOmitted: "omitted"}[phase]
	if phase == wf.NodePending {
		reason = map[string]string{"absent": "waiting to start", "image": "image pull failed", "unschedulable": "waiting for a node"}[state]
		if reason == "" {
			reason = "starting containers"
		}
	}
	if phase == wf.NodeFailed || phase == wf.NodeError {
		reason = map[string]string{"absent": "step failed", "creating": "failed before starting", "image": "image pull failed",
			"running": "step failed", "unschedulable": "waiting for a node", "config": "failed before starting",
			"exit": "exited with code 7", "oom": "out of memory", "init": "init container failed",
			"evicted": "Pod evicted", "disrupted": "Pod disrupted", "deadline": "step timed out", "shutdown": "node shut down"}[state]
	}
	if reason != "" && step.DisplayReason != reason {
		t.Fatalf("%s/%s: wanted %q, got %+v", phase, state, reason, step)
	}
	if phase == wf.NodePending && step.FailureClass != "" {
		t.Fatal("pending node claimed terminal failure", step)
	}
}
