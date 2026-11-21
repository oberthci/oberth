package argojob_test

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestClusterPodRefusalKeepsNativeDetail(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, message string
		class         stepmodel.FailureClass
		retry         stepmodel.RetryAction
	}{
		{"pod_security", `pods "step" is forbidden: violates PodSecurity "baseline:latest": privileged`, stepmodel.FailureAuthor, stepmodel.RetryNone},
		{"missing_account", `pods "step" is forbidden: error looking up service account runs/missing: serviceaccount "missing" not found`, stepmodel.FailureInfrastructure, stepmodel.RetryRerun},
		{"quota", `pods "step" is forbidden: exceeded quota: runs, requested: pods=1, used: pods=10, limited: pods=10`, stepmodel.FailureInfrastructure, stepmodel.RetryRerun},
	} {
		for _, phase := range []wf.NodePhase{wf.NodeFailed, wf.NodeError} {
			t.Run(tc.name+"/"+string(phase), func(t *testing.T) {
				w := nativeWorkflow()
				w.Status.Phase = wf.WorkflowFailed
				n := nativeNode("one", ".build", phase, 1)
				n.Message = tc.message
				w.Status.Nodes[n.ID] = n
				out := project(t, argojob.Projection{Workflow: w, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}})
				step := out.Steps[0]
				if step.Status != stepmodel.StepFailed || step.DisplayReason != "Pod refused by the cluster" || step.Detail != tc.message || out.Failure == nil {
					t.Fatal(out)
				}
				if out.Failure.Class != tc.class || out.Failure.Retry.Action != tc.retry {
					t.Fatalf("failure = %+v, want class %s and retry %s", out.Failure, tc.class, tc.retry)
				}
			})
		}
	}
}
