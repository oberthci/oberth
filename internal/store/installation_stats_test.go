package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestInstallationStatsCountsCatalogAndCompletedBackup(t *testing.T) {
	s := testStore(t)
	before, err := s.InstallationStats(t.Context())
	requireOK(t, err)
	if before.Upstreams != 0 || before.Repositories != 0 || !before.LastBackup.IsZero() {
		t.Fatal(before)
	}
	atomicRepository(t, s)
	a := stateport.Action{Actor: "operator"}
	requireOK(t, s.RecordAdministration(t.Context(), a, model.AuditAction{Actor: a.Actor, Action: "backup.completed", ResourceType: "installation"}))
	completed, err := s.InstallationStats(t.Context())
	requireOK(t, err)
	if completed.Upstreams != 1 || completed.Repositories != 1 || completed.LastBackup.IsZero() {
		t.Fatal(completed)
	}
	requireOK(t, s.RecordAdministration(t.Context(), a, model.AuditAction{Actor: a.Actor, Action: "backup.started", ResourceType: "installation"}))
	after, err := s.InstallationStats(t.Context())
	requireOK(t, err)
	if after != completed {
		t.Fatal("unfinished backup replaced completed time", after, completed)
	}
}
