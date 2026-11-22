package store

import (
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"reflect"
	"testing"
)

func TestSavedNativeProjectionSurvivesReopen(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "native-reopen")
	steps := []stepmodel.RunStep{{Path: "build", Name: "Compile", Status: stepmodel.StepFailed, DisplayReason: "exited with code 7", Detail: "full native diagnostic", AttemptCount: 1, Attempts: []stepmodel.StepAttempt{{NodeID: "node", PodName: "pod", Status: stepmodel.StepFailed}}, Logs: []stepmodel.LogSelector{{NodeID: "node", PodName: "pod", Container: "main"}}}}
	requireOK(t, s.SaveProgress(t.Context(), r.ID, stateport.Progress{Steps: steps}))
	requireOK(t, s.Close())
	reopened, err := Open(t.Context(), s.path, s.options)
	requireOK(t, err)
	defer func() { requireOK(t, reopened.Close()) }()
	got, err := reopened.Steps(t.Context(), r.ID)
	requireOK(t, err)
	if !reflect.DeepEqual(got, steps) {
		t.Fatal("display projection changed", got)
	}
	run, err := reopened.Run(t.Context(), r.ID)
	requireOK(t, err)
	if run.Failed != 1 || run.Pending != 0 || run.Revision != r.Revision {
		t.Fatal("projection counts or revision differ", run)
	}
}

// Progress after the verdict keeps the finished status and reason; progress
// never bumps the control revision and the status column follows the record.
func TestProgressAfterFinishKeepsTheVerdict(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "native-finished")
	steps := []stepmodel.RunStep{{Path: "build", Status: stepmodel.StepQueued}}
	requireOK(t, s.SaveProgress(t.Context(), r.ID, stateport.Progress{Steps: steps, Waiting: "waiting for a run slot"}))
	queued, err := s.Runs(t.Context(), stateport.RunFilter{Status: stepmodel.RunQueued, Limit: 10})
	requireOK(t, err)
	if len(queued) != 1 || queued[0].DisplayReason != "waiting for a run slot" || queued[0].Revision != r.Revision {
		t.Fatal("queued progress not listed by status", queued)
	}
	requireOK(t, s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed, Reason: "run failed"}))
	steps[0].Status = stepmodel.StepRunning
	requireOK(t, s.SaveProgress(t.Context(), r.ID, stateport.Progress{Steps: steps}))
	got, err := s.Run(t.Context(), r.ID)
	requireOK(t, err)
	if got.Status != stepmodel.RunFailed || got.DisplayReason != "run failed" {
		t.Fatal("progress changed a finished run", got.Status, got.DisplayReason)
	}
}
