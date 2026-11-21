package contracttest

import (
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"testing"
)

func checkSealedDecision(t *testing.T, f RunFixture) {
	b := batchFor(t, f, RunCase{ID: "decided"})
	// The gate's authority decision is not encoded as a second store permission check.
	b.Actor.Scope = []string{"foreign"}
	b.Runs[0].Request.Actor = b.Actor.Clone()
	f.Archive(t, b.Repository)
	recorded, err := f.Port.AcceptBatch(t.Context(), b)
	mustRun(t, err)
	if !recorded.Fresh || len(recorded.Runs) != 1 {
		t.Fatal("decided batch did not record", recorded)
	}
	stored, err := f.Port.Run(t.Context(), recorded.Runs[0].ID)
	mustRun(t, err)
	if stored.Admission.Digest != b.Runs[0].Prepared.View().Digest {
		t.Fatal("recording changed admission")
	}
	replay, err := f.Port.AcceptBatch(t.Context(), b)
	mustRun(t, err)
	if replay.Fresh || len(replay.Runs) != 1 {
		t.Fatal("archive or scope changed receipt replay")
	}
}

func checkAcceptanceRollbackResult(t *testing.T, f RunFixture) {
	r := acceptOneRun(t, f, RunCase{ID: "existing"})
	event := runEvent(r, "collision")
	mustRun(t, f.Emit(t.Context(), []stateport.EventWrite{event}))
	b := batchFor(t, f, RunCase{ID: "rolled-back"})
	event.Event.Summary = "different fact"
	b.Events = []stateport.EventWrite{event}
	ch := f.Port.Changes()
	head := len(f.Audit(t))
	failed, err := f.Port.AcceptBatch(t.Context(), b)
	runCode(t, err, publicerr.Conflict)
	if failed.Fresh || failed.Runs != nil {
		t.Fatal("rollback returned stored outcomes", failed)
	}
	unchangedSignal(t, ch)
	if len(f.Audit(t)) != head {
		t.Fatal("rollback retained audit")
	}
	_, err = f.Port.Run(t.Context(), "rolled-back")
	runCode(t, err, publicerr.NotFound)
	_, err = f.Port.AcceptedRuns(t.Context(), b.ReceiveID)
	runCode(t, err, publicerr.NotFound)
}
