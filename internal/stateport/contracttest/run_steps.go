package contracttest

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"reflect"
	"testing"
)

func checkPlanStepShape(t *testing.T, f RunFixture) {
	batch := batchFor(t, f, RunCase{ID: "plan-step-shape"})
	view := batch.Runs[0].Prepared.View()
	view.Plan.Entries = []admissioncontract.PlanEntry{{Path: "z-build", Name: "build", CredentialIntent: true}, {Path: "a-test", Name: "test"}}
	prepared, err := admissioncontract.Seal(view)
	mustRun(t, err)
	batch.Runs[0].Prepared = prepared
	recorded, err := f.Port.AcceptBatch(t.Context(), batch)
	mustRun(t, err)
	steps, err := f.Port.Steps(t.Context(), recorded.Runs[0].ID)
	mustRun(t, err)
	if len(steps) != 2 || steps[0].Path != "z-build" || steps[1].Path != "a-test" || !steps[0].CredentialIntent || steps[1].CredentialIntent {
		t.Fatal("steps lost plan order or credential intent", steps)
	}
	savedStepShape(t, f, recorded.Runs[0].ID, steps)
}

func checkNativeStepShape(t *testing.T, f RunFixture) {
	batch := batchFor(t, f, RunCase{ID: "native-step-shape"})
	view := batch.Runs[0].Prepared.View()
	view.Plan.Exact = false
	view.Plan.Entries = []admissioncontract.PlanEntry{{Path: "z-build", Name: "build"}, {Path: "loop", Name: "test", Dynamic: true, CredentialIntent: true}, {Path: "a-publish", Name: "publish"}}
	prepared, err := admissioncontract.Seal(view)
	mustRun(t, err)
	batch.Runs[0].Prepared = prepared
	saved, err := f.Port.AcceptBatch(t.Context(), batch)
	mustRun(t, err)
	id := saved.Runs[0].ID
	f.Nodes(t, id, []stateport.Node{
		{NodeID: "scaffold", Path: "executor/scaffold", Attempt: 1, Status: stepmodel.StepRunning},
		{NodeID: "z-child", Path: "loop/z", Attempt: 1, Status: stepmodel.StepRunning},
		{NodeID: "a-child", Path: "loop/a", Attempt: 1, Status: stepmodel.StepRunning},
	})
	steps, err := f.Port.Steps(t.Context(), id)
	mustRun(t, err)
	var paths []string
	for _, step := range steps {
		paths = append(paths, step.Path)
	}
	if !reflect.DeepEqual(paths, []string{"z-build", "loop", "loop/a", "loop/z", "a-publish"}) {
		t.Fatal("scaffolding or dynamic child order changed", paths)
	}
	for index, item := range []string{"a", "z"} {
		step := steps[index+2]
		if step.Name != "test ["+item+"]" || !step.CredentialIntent {
			t.Fatal("dynamic child lost plan name, item or credential intent", step)
		}
	}
	savedStepShape(t, f, id, steps)
}

func savedStepShape(t *testing.T, f RunFixture, id string, want []stepmodel.RunStep) {
	t.Helper()
	input := make([]stepmodel.RunStep, len(want))
	for i, s := range want {
		s.CredentialIntent = !s.CredentialIntent
		input[len(want)-1-i] = s.Clone()
	}
	mustRun(t, f.Port.SaveSteps(t.Context(), id, input))
	got, err := f.Port.Steps(t.Context(), id)
	mustRun(t, err)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("saved steps lost plan order, names or credential intent", got, want)
	}
}
