package argojob_test

import (
	"encoding/json"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestFailureRecordUsesPlanStepName(t *testing.T) {
	for _, tc := range []struct{ path, name string }{
		{"lint", "lint"},
		{"[1].third", "third"},
		{"group.test", "test"},
		{"onExit[0].cleanup", "cleanup"},
		{"unnamed", ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := nativeWorkflow()
			w.Status.Phase = wf.WorkflowFailed
			n := nativeNode("generated-node", "."+tc.path, wf.NodeFailed, 1)
			w.Status.Nodes[n.ID] = n
			plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: tc.path, Name: tc.name}}}
			out := project(t, argojob.Projection{Plan: plan, Workflow: w, Pods: []*core.Pod{nativePod(w, n, 7)}})
			if out.Failure == nil || out.Failure.Step != tc.path || out.Failure.Name != tc.name {
				t.Fatalf("failure = %+v, want path %q and name %q", out.Failure, tc.path, tc.name)
			}
			data, err := json.Marshal(stepmodel.RunSummary{Failure: out.Failure})
			if err != nil {
				t.Fatal(err)
			}
			var wire struct{ Failure map[string]any }
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatal(err)
			}
			if wire.Failure["failed_step"] != tc.path || wire.Failure["failed_step_name"] != tc.name {
				t.Fatalf("public JSON = %s", data)
			}
		})
	}
}
