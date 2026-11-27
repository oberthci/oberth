package contracttest

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func checkHoldsAndVerdict(t *testing.T, f RunFixture) {
	r := acceptOneRun(t, f, RunCase{ID: "native-verdict"})
	current, err := f.Port.RunState(t.Context(), r.ID)
	mustRun(t, err)
	checkNativeVerdict(t, f, current)
}

func checkNativeVerdict(t *testing.T, f RunFixture, r stateport.RunState) {
	phase := stepmodel.NativePhase("Error")
	u := stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed, Reason: "image pull failed", Detail: strings.Repeat("diagnostic ", 200), NativePhase: &phase}
	ch := observeChanges(t, f.Port)
	for _, bad := range []stateport.RunUpdate{
		{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed, Reason: strings.Repeat("x", 41)},
		{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed, NativePhase: new(stepmodel.NativePhase("unknown"))},
	} {
		runCode(t, f.Port.Finish(t.Context(), stateport.Action{}, bad), publicerr.InvalidInput)
		unchangedSignal(t, ch)
	}
	a := stateport.Action{Events: []stateport.EventWrite{runEvent(stateport.Run{RunSummary: r.RunSummary}, "finished")}}
	mustRun(t, f.Port.Finish(t.Context(), a, u))
	changedSignal(t, ch)
	got, err := f.Port.Run(t.Context(), r.ID)
	mustRun(t, err)
	if got.DisplayReason != u.Reason || got.Detail != u.Detail || got.NativePhase == nil || *got.NativePhase != phase {
		t.Fatal("native verdict was reconstructed or discarded", got)
	}
	ch = observeChanges(t, f.Port)
	runCode(t, f.Port.Finish(t.Context(), a, u), publicerr.Conflict)
	unchangedSignal(t, ch)
	page, err := f.Port.Events(t.Context(), stateport.EventFilter{Limit: 500})
	mustRun(t, err)
	if len(page.Events) != 1 {
		t.Fatal("native verdict events did not commit once", page)
	}
}
