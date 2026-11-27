package store

import (
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"testing"
)

func TestInstallAdminUnchangedBindingRecordsNothing(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	key := installRegistration(t, 'a')
	requireOK(t, s.InstallAdmin(t.Context(), installObservation("first"), key))
	assertInstallObservations(t, s, 1)
	before := databaseContents(t, s.readers)
	changes := observeChanges(t, s)
	requireOK(t, s.InstallAdmin(t.Context(), installObservation("repeated"), key))
	assertInstallObservations(t, s, 1)
	if databaseContents(t, s.readers) != before {
		t.Fatal("unchanged install binding changed durable state")
	}
	select {
	case <-changes:
		t.Fatal("unchanged install binding woke observers")
	default:
	}
}
func TestInstallAdminChangedBindingCommitsSuppliedEvents(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	requireOK(t, s.InstallAdmin(t.Context(), installObservation("first"), installRegistration(t, 'a')))
	assertInstallObservations(t, s, 1)
	changes := observeChanges(t, s)
	requireOK(t, s.InstallAdmin(t.Context(), installObservation("changed"), installRegistration(t, 'b')))
	assertInstallObservations(t, s, 2)
	select {
	case <-changes:
	default:
		t.Fatal("changed install binding did not wake observers")
	}
}
func installObservation(id string) stateport.Action {
	return stateport.Action{Actor: "installation", Events: []stateport.EventWrite{{Dedupe: "install:" + id, Event: model.AgentEvent{Kind: string(model.EventRunUpdated), Repo: "origin/team/repo", Summary: "run updated"}}}}
}
func assertInstallObservations(t *testing.T, s *Store, want int) {
	t.Helper()
	audits, head, err := s.Audit(t.Context(), 0, 200)
	requireOK(t, err)
	if len(audits) != want || head.ID != int64(want) {
		t.Fatal("install audit count", audits, head)
	}
	for _, a := range audits {
		if a.Action != "identity.install" || a.Actor != "installation" {
			t.Fatal("install audit lost action or actor", a)
		}
	}
	events, err := s.Events(t.Context(), stateport.EventFilter{Limit: 200})
	requireOK(t, err)
	if len(events.Events) != want || events.LatestID != int64(want) {
		t.Fatal("install event count", events)
	}
	for _, e := range events.Events {
		if e.Kind != string(model.EventRunUpdated) {
			t.Fatal("wrong install event", e)
		}
	}
}
