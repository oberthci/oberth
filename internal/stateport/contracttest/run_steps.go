package contracttest

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"reflect"
	"testing"
	"time"
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

// checkRunStart: a run starts when its first step starts; a later save never
// moves the start forward, an earlier observed start moves it back.
func checkRunStart(t *testing.T, f RunFixture) {
	saved, err := f.Port.AcceptBatch(t.Context(), batchFor(t, f, RunCase{ID: "run-start"}))
	mustRun(t, err)
	id := saved.Runs[0].ID
	steps, err := f.Port.Steps(t.Context(), id)
	mustRun(t, err)
	if r, err := f.Port.Run(t.Context(), id); err != nil || r.StartedAt != nil {
		t.Fatal("an unobserved run has a start", r.StartedAt, err)
	}
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, c := range []struct{ step, want time.Time }{{base, base}, {base.Add(time.Minute), base}, {base.Add(-time.Second), base.Add(-time.Second)}} {
		started := c.step.In(time.FixedZone("east", 3600))
		steps[0].StartedAt = &started
		mustRun(t, f.Port.SaveSteps(t.Context(), id, steps))
		r, err := f.Port.Run(t.Context(), id)
		mustRun(t, err)
		if r.StartedAt == nil || !r.StartedAt.Equal(c.want) || r.StartedAt.Location() != time.UTC {
			t.Fatal("run start is not its earliest step start in UTC", r.StartedAt, c.want)
		}
	}
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
