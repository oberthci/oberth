package contracttest

import (
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

func checkProgressFinish(t *testing.T, f RunFixture) {
	r := acceptOneRun(t, f, RunCase{ID: "progress-finish"})
	f.NativeAdvance(t, r.ID)
	mustRun(t, f.Port.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed}))
	finished, err := f.Port.RunState(t.Context(), r.ID)
	mustRun(t, err)
	if finished.Status != stepmodel.RunFailed || finished.Revision != r.Revision+1 {
		t.Fatal("progress defeated or replaced terminal finish", finished)
	}
}
