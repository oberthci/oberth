package argojob_test

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepmodel"
	"reflect"
	"testing"
)

func TestProjectionKeepsPlanOrderAndCredentialIntent(t *testing.T) {
	t.Parallel()
	for _, dynamic := range []bool{false, true} {
		t.Run(map[bool]string{false: "static", true: "dynamic"}[dynamic], func(t *testing.T) {
			w := nativeWorkflow()
			plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "z-build", Name: "build", CredentialIntent: true, Dynamic: dynamic}, {Path: "a-test", Name: "test"}}}
			paths := []string{"z-build", "a-test"}
			if dynamic {
				paths = []string{"z-build(0:a)", "z-build(1:z)", "a-test"}
			}
			for i, path := range paths {
				n := nativeNode(path, "."+path, wf.NodeRunning, int64(len(paths)-i))
				w.Status.Nodes[n.ID] = n
			}
			before := project(t, argojob.Projection{Plan: plan, Workflow: w})
			assertPlanProjection(t, before.Steps, paths)
			// The admitted plan determines display facts when prior observations disagree.
			for i := range before.Steps {
				before.Steps[i].CredentialIntent = !before.Steps[i].CredentialIntent
			}
			w.Status.Nodes = nil
			after := project(t, argojob.Projection{Plan: plan, Workflow: w, Previous: before.Steps, PreviousNodes: before.Nodes})
			assertPlanProjection(t, after.Steps, paths)
		})
	}
}
func assertPlanProjection(t *testing.T, steps []stepmodel.RunStep, paths []string) {
	t.Helper()
	got := []string{}
	for _, s := range steps {
		got = append(got, s.Path)
		if s.CredentialIntent != (s.Path != "a-test") {
			t.Fatal("plan credential intent lost", steps)
		}
	}
	if !reflect.DeepEqual(got, paths) {
		t.Fatal("plan order lost", got, paths)
	}
}
