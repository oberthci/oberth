package contracttest

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func checkCancellation(t *testing.T, f RunFixture) {
	r := acceptOneRun(t, f, RunCase{ID: "cancel"})
	a := stateport.Action{Actor: "operator", Reason: "requested stop", Events: []stateport.EventWrite{runEvent(r, "stop")}}
	bad := a
	bad.Events = []stateport.EventWrite{runEvent(r, "")}
	ch := f.Port.Changes()
	head := len(f.Audit(t))
	_, err := f.Port.Cancel(t.Context(), bad, stateport.RunCancellation{ID: r.ID, Revision: r.Revision})
	runCode(t, err, publicerr.InvalidInput)
	unchangedSignal(t, ch)
	current, err := f.Port.RunState(t.Context(), r.ID)
	mustRun(t, err)
	if current.Revision != r.Revision || len(f.Audit(t)) != head {
		t.Fatal("failed cancellation committed a partial fact")
	}
	stop, err := f.Port.Cancel(t.Context(), a, stateport.RunCancellation{ID: r.ID, Revision: r.Revision})
	mustRun(t, err)
	if stop.Value != "Stop" {
		t.Fatal("first cancellation did not return Stop", stop)
	}
	changedSignal(t, ch)
	_, err = f.Port.Cancel(t.Context(), a, stateport.RunCancellation{ID: r.ID, Revision: r.Revision})
	runCode(t, err, publicerr.Conflict)
	current, err = f.Port.RunState(t.Context(), r.ID)
	mustRun(t, err)
	a.Reason = "requested escalation"
	a.Events = []stateport.EventWrite{runEvent(r, "terminate")}
	stop, err = f.Port.Cancel(t.Context(), a, stateport.RunCancellation{ID: r.ID, Revision: current.Revision})
	mustRun(t, err)
	if stop.Value != "Terminate" {
		t.Fatal("second cancellation did not escalate", stop)
	}
	audit := f.Audit(t)
	if !strings.Contains(audit[len(audit)-1].Details, a.Reason) {
		t.Fatal("stop attribution lost reason", audit)
	}
	current, err = f.Port.RunState(t.Context(), r.ID)
	mustRun(t, err)
	mustRun(t, f.Port.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: r.ID, Revision: current.Revision, Status: stepmodel.RunFailed}))
	current, err = f.Port.RunState(t.Context(), r.ID)
	mustRun(t, err)
	ch = f.Port.Changes()
	head = len(f.Audit(t))
	_, err = f.Port.Cancel(t.Context(), a, stateport.RunCancellation{ID: r.ID, Revision: current.Revision})
	runCode(t, err, publicerr.Conflict)
	unchangedSignal(t, ch)
	if len(f.Audit(t)) != head {
		t.Fatal("terminal cancellation added audit")
	}
	immediate := acceptOneRun(t, f, RunCase{ID: "immediate"})
	stop, err = f.Port.Cancel(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunCancellation{ID: immediate.ID, Revision: immediate.Revision, Terminate: true})
	mustRun(t, err)
	if stop.Value != "Terminate" {
		t.Fatal("explicit termination downgraded")
	}
}

func checkCancellationRace(t *testing.T, f RunFixture) {
	r := acceptOneRun(t, f, RunCase{ID: "race"})
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := f.Port.Cancel(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunCancellation{ID: r.ID, Revision: r.Revision})
			results <- err
		}()
	}
	close(start)
	wins := 0
	for range 2 {
		err := <-results
		if err == nil {
			wins++
		} else {
			runCode(t, err, publicerr.Conflict)
		}
	}
	if wins != 1 {
		t.Fatal("prepared run revision had multiple winners", wins)
	}
	rows, err := f.Port.Obligations(t.Context(), r.ID)
	mustRun(t, err)
	if len(rows) != 1 || rows[0].Value != "Stop" {
		t.Fatal("losing cancellation escalated intent", rows)
	}
}

func checkStaleVerdict(t *testing.T, f RunFixture) {
	r := acceptOneRun(t, f, RunCase{ID: "stale-verdict"})
	_, err := f.Port.Cancel(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunCancellation{ID: r.ID, Revision: r.Revision})
	mustRun(t, err)
	ch := f.Port.Changes()
	head := len(f.Audit(t))
	err = f.Port.Finish(t.Context(), stateport.Action{Events: []stateport.EventWrite{runEvent(r, "stale-finished")}}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed})
	runCode(t, err, publicerr.Conflict)
	unchangedSignal(t, ch)
	after, err := f.Port.RunState(t.Context(), r.ID)
	mustRun(t, err)
	if after.Status != r.Status || len(f.Audit(t)) != head {
		t.Fatal("stale verdict changed native work")
	}
	page, err := f.Port.Events(t.Context(), stateport.EventFilter{Limit: 500})
	mustRun(t, err)
	if len(page.Events) != 0 {
		t.Fatal("stale verdict wrote events")
	}
}
