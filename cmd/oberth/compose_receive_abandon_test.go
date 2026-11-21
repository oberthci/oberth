//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestComposedArchivedReceiveIsAbandoned(t *testing.T) {
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindClient(t)
	pending := f.leavePendingReceive(t)
	repo, err := f.owners.c.store.Repository(t.Context(), gittest.Repository, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := (serverPeopleStore{f.owners.c.store, f.owners}).ArchiveRepository(t.Context(), stateport.Action{Actor: "operator"}, repo.ID); err != nil {
		t.Fatal(err)
	}
	f.assertReceiveAbandoned(t, pending)
}

func (f *composedFixture) assertReceiveAbandoned(t *testing.T, pending gitcache.Reservation) {
	t.Helper()
	if _, err := f.owners.cache.PendingReceive(t.Context(), pending.Event.Repository); !os.IsNotExist(err) {
		t.Fatal("invalid authority left a pending receive", err)
	}
	runs, err := f.owners.c.store.Runs(t.Context(), stateport.RunFilter{Limit: 200})
	if err != nil || len(runs) != 0 {
		t.Fatal("abandoned receive ran", runs, err)
	}
	audit, _, err := f.owners.c.store.Audit(t.Context(), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audit {
		if a.Action == "receive.abandon" && a.ResourceID == pending.Event.ID && strings.Contains(a.Details, "reason") {
			found = true
		}
	}
	if !found {
		t.Fatal("automatic abandonment was not audited")
	}
}
