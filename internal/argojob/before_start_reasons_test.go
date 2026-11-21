package argojob_test

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	"testing"
)

func TestEndedStepKeepsTypedReasonAndRetryHint(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		class        stepmodel.FailureClass
		retry        stepmodel.RetryAction
		change       func(*core.Pod)
	}{
		{"image", "image pull failed", stepmodel.FailureUnknown, stepmodel.RetryNone, func(p *core.Pod) { p.Status.ContainerStatuses[0].State.Waiting.Reason = "ErrImagePull" }},
		{"deadline", "step timed out", stepmodel.FailureDeadline, stepmodel.RetryNone, func(p *core.Pod) { p.Status.Reason = "DeadlineExceeded" }},
		{"node", "waiting for a node", stepmodel.FailureUnknown, stepmodel.RetryNone, func(p *core.Pod) {
			p.Status.Conditions = []core.PodCondition{{Type: core.PodScheduled, Status: core.ConditionFalse, Reason: "Unschedulable"}}
		}},
		{"init", "init container failed", stepmodel.FailureAuthor, stepmodel.RetryNone, func(p *core.Pod) {
			p.Status.InitContainerStatuses = []core.ContainerStatus{{Name: "authored-init", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 7}}}}
		}},
		{"generic", "failed before starting", stepmodel.FailureInfrastructure, stepmodel.RetryRerun, func(*core.Pod) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := nativeWorkflow()
			w.Status.Phase = wf.WorkflowFailed
			n := nativeNode("one", ".build", wf.NodeError, 1)
			w.Status.Nodes[n.ID] = n
			p := nativePod(w, n, 0)
			p.Status.ContainerStatuses[0].State = core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "ContainerCreating"}}
			tc.change(p)
			out := project(t, argojob.Projection{Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build", Name: "build"}}}, Workflow: w, Pods: []*core.Pod{p}})
			if out.Failure == nil {
				t.Fatal("failure missing", out)
			}
			if out.Reason != tc.reason || out.Failure.Class != tc.class || out.Failure.Retry.Action != tc.retry {
				t.Fatal("typed diagnosis replaced", out.Reason, out.Failure)
			}
		})
	}
}
