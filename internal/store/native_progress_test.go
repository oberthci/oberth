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
