package contracttest

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"reflect"
	"slices"
	"testing"
	"time"
)

func checkPlanStepShape(t *testing.T, f RunFixture) {
	batch := batchFor(t, f, RunCase{ID: "plan-step-shape"})
	view := batch.Runs[0].Prepared.View()
	view.Plan.Entries = []admissioncontract.PlanEntry{{Path: "z-build", Name: "build", CredentialIntent: true, ParallelGroup: 1}, {Path: "a-test", Name: "test", ParallelGroup: 1}}
	prepared, err := admissioncontract.Seal(view)
	mustRun(t, err)
	batch.Runs[0].Prepared = prepared
	recorded, err := f.Port.AcceptBatch(t.Context(), batch)
	mustRun(t, err)
	steps, err := f.Port.Steps(t.Context(), recorded.Runs[0].ID)
	mustRun(t, err)
	if len(steps) != 2 || steps[0].Path != "z-build" || steps[1].Path != "a-test" || !steps[0].CredentialIntent || steps[1].CredentialIntent || steps[0].ParallelGroup != 1 || steps[1].ParallelGroup != 1 {
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

// checkRunStart: a run is queued, without a start, while its steps only wait or
// are skipped;
// it runs from the first step that starts, whose start is the run's, and never
// queues again. A later save never moves the start forward, an earlier
// observed start moves it back. A queued run is listed by its status, progress
// keeps the control revision, and a finished run keeps its verdict.
func checkRunStart(t *testing.T, f RunFixture) {
	saved, err := f.Port.AcceptBatch(t.Context(), batchFor(t, f, RunCase{ID: "run-start"}))
	mustRun(t, err)
	id := saved.Runs[0].ID
	steps, err := f.Port.Steps(t.Context(), id)
	mustRun(t, err)
	if r, err := f.Port.Run(t.Context(), id); err != nil || r.StartedAt != nil || r.Status != stepmodel.RunQueued {
		t.Fatal("an unobserved run has a start or is not queued", r.StartedAt, r.Status, err)
	}
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	waited := base.Add(-time.Hour)
	steps[0].Status, steps[0].StartedAt = stepmodel.StepSkipped, &waited
	mustRun(t, f.Port.SaveProgress(t.Context(), id, stateport.Progress{Steps: steps}))
	runState(t, f, id, progressWant{stepmodel.RunQueued, "", nil})
	steps[0].Status, steps[0].StartedAt = stepmodel.StepQueued, &waited
	if err = f.Port.SaveProgress(t.Context(), id, stateport.Progress{Steps: steps, Waiting: "waiting for a run slot that is far too long"}); err == nil {
		t.Fatal("a waiting reason over 40 characters was saved")
	}
	mustRun(t, f.Port.SaveProgress(t.Context(), id, stateport.Progress{Steps: steps, Waiting: "waiting for sync limit go"}))
	runState(t, f, id, progressWant{stepmodel.RunQueued, "waiting for sync limit go", nil})
	queued, err := f.Port.Runs(t.Context(), stateport.RunFilter{Status: stepmodel.RunQueued, Limit: 100})
	mustRun(t, err)
	if !slices.ContainsFunc(queued, func(r stateport.Run) bool { return r.ID == id && r.DisplayReason == "waiting for sync limit go" }) {
		t.Fatal("a queued run is not listed by its status", queued)
	}
	for _, c := range []struct{ step, want time.Time }{{base, base}, {base.Add(time.Minute), base}, {base.Add(-time.Second), base.Add(-time.Second)}} {
		started := c.step.In(time.FixedZone("east", 3600))
		steps[0].Status, steps[0].StartedAt = stepmodel.StepRunning, &started
		mustRun(t, f.Port.SaveProgress(t.Context(), id, stateport.Progress{Steps: steps, Waiting: "waiting for sync limit go"}))
		runState(t, f, id, progressWant{stepmodel.RunRunning, "", &c.want})
	}
	steps[0].Status = stepmodel.StepQueued
	mustRun(t, f.Port.SaveProgress(t.Context(), id, stateport.Progress{Steps: steps, Waiting: "waiting for a node"}))
	runState(t, f, id, progressWant{stepmodel.RunRunning, "", new(base.Add(-time.Second))})
	mustRun(t, f.Port.Finish(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunUpdate{ID: id, Revision: saved.Runs[0].Revision, Status: stepmodel.RunFailed, Reason: "run failed"}))
	steps[0].Status = stepmodel.StepRunning
	mustRun(t, f.Port.SaveProgress(t.Context(), id, stateport.Progress{Steps: steps, Waiting: "waiting for a node"}))
	r, err := f.Port.Run(t.Context(), id)
	mustRun(t, err)
	if r.Status != stepmodel.RunFailed || r.DisplayReason != "run failed" {
		t.Fatal("progress changed a finished run", r.Status, r.DisplayReason)
	}
}

type progressWant struct {
	status stepmodel.RunStatus
	reason string
	start  *time.Time
}

func runState(t *testing.T, f RunFixture, id string, want progressWant) {
	t.Helper()
	r, err := f.Port.Run(t.Context(), id)
	mustRun(t, err)
	if r.Status != want.status || r.DisplayReason != want.reason || (want.start == nil) != (r.StartedAt == nil) {
		t.Fatal("run progress differs", r.Status, r.DisplayReason, r.StartedAt, want)
	}
	if want.start != nil && (!r.StartedAt.Equal(*want.start) || r.StartedAt.Location() != time.UTC) {
		t.Fatal("run start is not its earliest started step start in UTC", r.StartedAt, *want.start)
	}
}

func savedStepShape(t *testing.T, f RunFixture, id string, want []stepmodel.RunStep) {
	t.Helper()
	input := make([]stepmodel.RunStep, len(want))
	for i, s := range want {
		s.CredentialIntent = !s.CredentialIntent
		s.ParallelGroup = 99
		input[len(want)-1-i] = s.Clone()
	}
	mustRun(t, f.Port.SaveProgress(t.Context(), id, stateport.Progress{Steps: input}))
	got, err := f.Port.Steps(t.Context(), id)
	mustRun(t, err)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("saved steps lost plan order, names or credential intent", got, want)
	}
}
