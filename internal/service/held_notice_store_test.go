package service_test

import (
	"bytes"
	"database/sql"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
	"testing"
	"time"
)

func realNoticeFixture(t *testing.T) (*fixture, *store.Store, string, model.HeldNotice) {
	t.Helper()
	f := setup(t)
	state, path := landedStoreWithPath(t, f)
	landedRepository(t, state)
	b := batch(t)
	b.Runs[0].Request.Assist = true
	b.Runs[0].Prepared = landedAdmission(t, runID, b.Runs[0].Request)
	if _, err := state.AcceptBatch(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	useNoticeStore(t, f, state)
	return f, state, path, model.HeldNotice{RunID: runID, Step: "build", NodeID: "native-node", PodUID: "native-pod-uid", HeldSince: at().Add(-5 * time.Minute), TTLSeconds: 1200}
}
func useNoticeStore(t *testing.T, f *fixture, state *store.Store) {
	t.Helper()
	f.c.Runs, f.c.Catalog, f.c.Events = state, state, state
	f.c.HoldStore, f.c.Publications, f.c.Promotions = state, state, state
	f.restart(t)
}
func heldEvent(t *testing.T, state *store.Store) model.AgentEvent {
	t.Helper()
	page, err := state.Events(t.Context(), stateport.EventFilter{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	found := []model.AgentEvent{}
	for _, e := range page.Events {
		if e.Kind == string(model.EventRunHeld) {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatal("held notice was missing or duplicated", found)
	}
	return found[0]
}
func TestBImmutableHeldEventRealStore(t *testing.T) {
	t.Parallel()
	f, state, path, h := realNoticeFixture(t)
	before, err := state.Run(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.Held(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	first := heldEvent(t, state)
	f.offset.Store(int64(2 * time.Hour))
	if err = f.s.Held(t.Context(), h); err != nil {
		t.Fatal("replay at another server clock refused", err)
	}
	repeated := heldEvent(t, state)
	if first.ID != repeated.ID || !first.At.Equal(h.HeldSince) || !bytes.Equal(first.Data, repeated.Data) {
		t.Fatal(first, repeated)
	}
	checkNoticeControl(t, state, before)
	if err = f.s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = state.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := reopenNoticeStore(t, f, path)
	if err = f.s.Held(t.Context(), h); err != nil {
		t.Fatal("restart replay refused", err)
	}
	replayed := heldEvent(t, reopened)
	if replayed.ID != first.ID || !bytes.Equal(replayed.Data, first.Data) {
		t.Fatal("restart changed immutable event", replayed)
	}
}
func TestBRefusedNoticeDoesNotBlockFinalResult(t *testing.T) {
	t.Parallel()
	f, state, path, h := realNoticeFixture(t)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_, err = raw.ExecContext(t.Context(), `CREATE TRIGGER refuse_notice BEFORE INSERT ON events WHEN NEW.kind='run.held' BEGIN SELECT RAISE(ABORT,'notice refused'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.Held(t.Context(), h); err == nil {
		t.Fatal("real store did not refuse notice")
	}
	outcome := service.FinishedRun{RunID: runID, Status: stepmodel.RunFailed, NativePhase: new(stepmodel.NativeFailed), Reason: "step failed", Detail: "native detail"}
	if err = f.s.Finished(t.Context(), outcome); err != nil {
		t.Fatal("notice failure blocked native result", err)
	}
	r, err := state.Run(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != outcome.Status || r.Detail != outcome.Detail {
		t.Fatal(r)
	}
	page, err := state.Events(t.Context(), stateport.EventFilter{Limit: 200})
	if err != nil || countEventKind(page, model.EventRunHeld) != 0 || countEventKind(page, model.EventRunFinished) != 1 {
		t.Fatal(page, err)
	}
	if _, err = raw.ExecContext(t.Context(), `DROP TRIGGER refuse_notice`); err != nil {
		t.Fatal(err)
	}
}

func reopenNoticeStore(t *testing.T, f *fixture, path string) *store.Store {
	t.Helper()
	reopened, err := store.Open(t.Context(), path, store.Options{Clock: f.c.Now, Protocol: stateport.DefaultProtocol()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	useNoticeStore(t, f, reopened)
	return reopened
}

func checkNoticeControl(t *testing.T, state *store.Store, before stateport.Run) {
	t.Helper()
	after, err := state.Run(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.Status != before.Status {
		t.Fatal("notice changed run control or result", after)
	}
}
