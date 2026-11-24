package argojob_test

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestRunningStepUsesRunningReasonForStalePod(t *testing.T) {
	t.Parallel()
	for _, phase := range []wf.NodePhase{wf.NodeRunning, wf.NodePending} {
		for _, state := range []string{"absent", "creating", "image", "running", "exit", "oom",
			"init", "evicted", "disrupted", "deadline", "shutdown", "unschedulable", "config"} {
			t.Run(string(phase)+"/"+state, func(t *testing.T) {
				first := project(t, podPhaseInput(wf.NodeRunning, "running"))
				in := podPhaseInput(phase, state)
				in.Previous, in.PreviousNodes = first.Steps, first.Nodes
				for id, node := range in.Workflow.Status.Nodes {
					node.SynchronizationStatus = &wf.NodeSynchronizationStatus{Waiting: "ci/Mutex/forge/team/app/publish"}
					in.Workflow.Status.Nodes[id] = node
				}
				out := project(t, in)
				if step := out.Steps[0]; step.Status != stepmodel.StepRunning || step.DisplayReason != "running" {
					t.Fatal("running step retained a stale waiting reason", step)
				}
			})
		}
	}
}
