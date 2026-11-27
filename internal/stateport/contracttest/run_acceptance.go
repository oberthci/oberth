package contracttest

import (
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func checkRunAcceptance(t *testing.T, f RunFixture) {
	b := batchFor(t, f, RunCase{ID: "one"})
	recorded, err := f.Port.AcceptBatch(t.Context(), b)
	rows, fresh := recorded.Runs, recorded.Fresh
	mustRun(t, err)
	if !fresh || len(rows) != 1 {
		t.Fatal("acceptance missing")
	}
	first := rows[0]
	rows[0].Admission.Plan.Entries[0].Name = "changed"
	*rows[0].CommitSHA = "changed"
	read, err := f.Port.Run(t.Context(), first.ID)
	mustRun(t, err)
	if read.Admission.Digest != b.Runs[0].Prepared.View().Digest || *read.CommitSHA != b.Runs[0].Prepared.View().CommitSHA {
		t.Fatal("returned values alias storage")
	}
	head := len(f.Audit(t))
	ch := observeChanges(t, f.Port)
	replayed, err := f.Port.AcceptBatch(t.Context(), b)
	replay, fresh := replayed.Runs, replayed.Fresh
	mustRun(t, err)
	if fresh || len(replay) != 1 || replay[0].ID != first.ID || len(f.Audit(t)) != head {
		t.Fatal("replay repeated authority or work")
	}
	unchangedSignal(t, ch)
	b.Actor.Identity = "other"
	if _, err = f.Port.AcceptBatch(t.Context(), b); err == nil {
		t.Fatal("changed receipt accepted")
	}
	empty := stateport.BatchAcceptance{ReceiveID: "empty", Repository: first.Repo, Actor: b.Runs[0].Request.Actor}
	restoredResult, err := f.Port.AcceptBatch(t.Context(), empty)
	fresh = restoredResult.Fresh
	mustRun(t, err)
	if !fresh {
		t.Fatal("empty receipt missing")
	}
	saved, err := f.Port.AcceptedRuns(t.Context(), "empty")
	mustRun(t, err)
	if len(saved) != 0 {
		t.Fatal("empty receipt allocated a run")
	}
	_, err = f.Port.AcceptedRuns(t.Context(), "absent")
	runCode(t, err, publicerr.NotFound)
}

func checkCommitWakeups(t *testing.T, f RunFixture) {
	r := acceptOneRun(t, f, RunCase{ID: "wake"})
	ch := observeChanges(t, f.Port)
	page, err := f.Port.Events(t.Context(), stateport.EventFilter{Limit: 500})
	mustRun(t, err)
	f.NativeAdvance(t, r.ID)
	changedSignal(t, ch)
	current, err := f.Port.RunState(t.Context(), r.ID)
	mustRun(t, err)
	if current.Revision != r.Revision {
		t.Fatal("native progress invalidated a prepared run fact")
	}
	after, err := f.Port.Events(t.Context(), stateport.EventFilter{Limit: 500})
	mustRun(t, err)
	if len(after.Events) != len(page.Events) {
		t.Fatal("native progress invented an event")
	}
	ch = observeChanges(t, f.Port)
	_, err = f.Port.Cancel(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunCancellation{ID: r.ID, Revision: r.Revision})
	mustRun(t, err)
	changedSignal(t, ch)
}
