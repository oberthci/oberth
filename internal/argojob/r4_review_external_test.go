package argojob_test

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
)

func TestReviewR3NativeProjectionRetainsCredentialIntent(t *testing.T) {
	t.Parallel()
	for _, dynamic := range []bool{false, true} {
		t.Run(map[bool]string{false: "static", true: "dynamic"}[dynamic], func(t *testing.T) {
			w := nativeWorkflow()
			suffix := ".build"
			if dynamic {
				suffix = ".build(0:item)"
			}
			n := nativeNode("one", suffix, wf.NodeRunning, 1)
			w.Status.Nodes[n.ID] = n
			plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build", Name: "build", Dynamic: dynamic, CredentialIntent: true}}}
			o := project(t, argojob.Projection{Workflow: w, Plan: plan})
			if len(o.Steps) != 1 || !o.Steps[0].CredentialIntent {
				t.Fatalf("admitted credential intent disappears in native projection: steps=%+v", o.Steps)
			}
		})
	}
}
