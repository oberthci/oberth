package contracttest

import (
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func checkQueuedReason(t *testing.T, f RunFixture) {
	saved, err := f.Port.AcceptBatch(t.Context(), batchFor(t, f, RunCase{ID: "queued-reason"}))
	mustRun(t, err)
	id := saved.Runs[0].ID
	steps, err := f.Port.Steps(t.Context(), id)
	mustRun(t, err)
	steps[0].Status = stepmodel.StepPending
	for _, reason := range []string{"starting containers", "waiting to start", "waiting for a run slot", "waiting for sync limit go", ""} {
		mustRun(t, f.Port.SaveProgress(t.Context(), id, stateport.Progress{Steps: steps, Waiting: reason}))
		runState(t, f, id, progressWant{stepmodel.RunQueued, reason, nil})
	}
	steps[0].Status = stepmodel.StepRunning
	mustRun(t, f.Port.SaveProgress(t.Context(), id, stateport.Progress{Steps: steps, Waiting: "waiting to start"}))
	runState(t, f, id, progressWant{stepmodel.RunRunning, "", nil})
}
