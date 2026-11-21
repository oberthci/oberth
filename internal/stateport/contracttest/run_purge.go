package contracttest

import (
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func checkRunPurge(t *testing.T, f RunFixture) {
	r := acceptOneRun(t, f, RunCase{ID: "purge"})
	steps, err := f.Port.Steps(t.Context(), r.ID)
	mustRun(t, err)
	if len(steps) != 1 || steps[0].Path != "build" || steps[0].Status != stepmodel.StepPending {
		t.Fatal("admitted steps missing", steps)
	}
	a := stateport.Action{Actor: "operator"}
	runCode(t, f.Port.BeginPurge(t.Context(), a, r.ID, "receipt"), publicerr.Conflict)
	mustRun(t, f.Port.Finish(t.Context(), a, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed}))
	mustRun(t, f.Port.BeginPurge(t.Context(), a, r.ID, "receipt"))
	ch := f.Port.Changes()
	mustRun(t, f.Port.BeginPurge(t.Context(), a, r.ID, "receipt"))
	unchangedSignal(t, ch)
	runCode(t, f.Port.FinishPurge(t.Context(), a, r.ID, "different"), publicerr.Conflict)
	unchangedSignal(t, ch)
	before, err := f.Port.PurgeReceipt(t.Context(), r.ID)
	mustRun(t, err)
	if before.Completed {
		t.Fatal("failed acknowledgement completed purge")
	}
	mustRun(t, f.Port.FinishPurge(t.Context(), a, r.ID, "receipt"))
	changedSignal(t, ch)
	after, err := f.Port.PurgeReceipt(t.Context(), r.ID)
	mustRun(t, err)
	if !after.Completed || after.ID != "receipt" {
		t.Fatal("purge acknowledgement missing", after)
	}
}
