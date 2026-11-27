package contracttest

import (
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func checkRunEvents(t *testing.T, f RunFixture) {
	own := acceptOneRun(t, f, RunCase{ID: "own", Actor: "alice"})
	other := acceptOneRun(t, f, RunCase{ID: "other", Actor: "bob"})
	events := make([]stateport.EventWrite, 0, 1000)
	for i := range 500 {
		events = append(events, runEvent(other, fmt.Sprint("foreign-", i)), runEvent(own, fmt.Sprint("own-", i)))
	}
	ch := observeChanges(t, f.Port)
	mustRun(t, f.Emit(t.Context(), events))
	changedSignal(t, ch)
	q := stateport.EventFilter{Actor: "alice", Limit: 500}
	page, err := f.Port.Events(t.Context(), q)
	mustRun(t, err)
	if len(page.Events) != 500 {
		t.Fatal("actor filtering occurred after page limit", len(page.Events))
	}
	for _, e := range page.Events {
		if e.RunID != own.ID {
			t.Fatal("foreign actor event exposed")
		}
	}
	q.Through = page.Events[9].ID
	page, err = f.Port.Events(t.Context(), q)
	mustRun(t, err)
	if len(page.Events) != 10 {
		t.Fatal("event fence ignored", len(page.Events))
	}
	checkRunEventCursor(t, f, q, page)
	q.Limit = 501
	_, err = f.Port.Events(t.Context(), q)
	runCode(t, err, publicerr.InvalidInput)
}

func checkRunEventCursor(t *testing.T, f RunFixture, q stateport.EventFilter, page model.EventPage) {
	t.Helper()
	q.After, q.Limit = page.Events[4].ID, 2
	wantFirst, wantLast := page.Events[5].ID, page.Events[6].ID
	page, err := f.Port.Events(t.Context(), q)
	mustRun(t, err)
	if len(page.Events) != 2 || page.Events[0].ID != wantFirst || page.NextAfterID != wantLast || !page.HasMore {
		t.Fatal("cursor ownership, page limit or fence changed", page)
	}
	q.After, q.Limit = q.Through, 500
	page, err = f.Port.Events(t.Context(), q)
	mustRun(t, err)
	if len(page.Events) != 0 || page.NextAfterID != q.Through || page.HasMore {
		t.Fatal("empty cursor window escaped its fence", page)
	}
}

func checkInitialEvents(t *testing.T, f RunFixture) {
	empty, err := f.Port.Events(t.Context(), stateport.EventFilter{Limit: 500})
	mustRun(t, err)
	if empty.Gap {
		t.Fatal("fresh empty store reported a gap")
	}
	own := acceptOneRun(t, f, RunCase{ID: "initial", Actor: "alice"})
	f.Repository(t, "hidden/other/repo")
	other := acceptOneRun(t, f, RunCase{ID: "hidden-initial", Repo: "hidden/other/repo", Actor: "bob"})
	mustRun(t, f.Emit(t.Context(), []stateport.EventWrite{runEvent(other, "hidden-first"), runEvent(own, "own-initial"), runEvent(own, "own-later")}))
	q := stateport.EventFilter{Repositories: []string{own.Repo}, Limit: 500}
	page, err := f.Port.Events(t.Context(), q)
	mustRun(t, err)
	if page.Gap || page.OldestID <= 1 || len(page.Events) != 2 {
		t.Fatal("unpruned scoped history reported a gap", page)
	}
	q.Actor = "alice"
	page, err = f.Port.Events(t.Context(), q)
	mustRun(t, err)
	if page.Gap || len(page.Events) != 2 {
		t.Fatal("actor-filtered initial history reported a gap", page)
	}
	f.Prune(t)
	page, err = f.Port.Events(t.Context(), q)
	mustRun(t, err)
	if page.Gap || len(page.Events) != 0 || page.LatestID == 0 {
		t.Fatal("zero cursor did not start retained history", page)
	}
	q.After = 1
	page, err = f.Port.Events(t.Context(), q)
	mustRun(t, err)
	if !page.Gap {
		t.Fatal("nonzero resume cursor hid pruned history", page)
	}
	q.Repositories = []string{"unknown/team/repo"}
	page, err = f.Port.Events(t.Context(), q)
	mustRun(t, err)
	if page.Gap {
		t.Fatal("pruning escaped repository scope")
	}
}
