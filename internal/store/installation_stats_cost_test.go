package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestInstallationStatusQueryDoesNotReadAudit(t *testing.T) {
	s := testStore(t)
	rows, err := s.readers.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+installationCountsSQL)
	requireOK(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		requireOK(t, rows.Scan(&id, &parent, &unused, &detail))
		if strings.Contains(strings.ToLower(detail), "audit") {
			t.Fatal("status query reads audit history", detail)
		}
	}
	requireOK(t, rows.Err())
}

func TestBackupSnapshotTracksOnlyCommittedCompletionsAndReopens(t *testing.T) {
	var tick atomic.Int64
	tick.Store(1)
	options := Options{Protocol: stateport.DefaultProtocol(), Clock: func() time.Time { return time.Unix(tick.Load(), 0).UTC() }}
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Create(t.Context(), path, options)
	requireOK(t, err)
	t.Cleanup(func() { _ = s.Close() })
	record := func(ctx context.Context, action string) error {
		return s.RecordAdministration(ctx, stateport.Action{Actor: "operator"}, model.AuditAction{Action: action, ResourceType: "installation"})
	}
	requireOK(t, record(t.Context(), "backup.completed"))
	tick.Store(2)
	requireOK(t, record(t.Context(), "backup.started"))
	tick.Store(3)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := record(canceled, "backup.completed"); err == nil {
		t.Fatal("canceled completion recorded")
	}
	event := atomicEvent(s, "backup-receipt")
	action := stateport.Action{Actor: "operator", Events: []stateport.EventWrite{event}}
	requireOK(t, s.RecordAdministration(t.Context(), action, model.AuditAction{Action: "backup.started", ResourceType: "installation"}))
	tick.Store(4)
	action.Events[0].Event.At = action.Events[0].Event.At.Add(time.Second)
	if err := s.RecordAdministration(t.Context(), action, model.AuditAction{Action: "backup.completed", ResourceType: "installation"}); err == nil {
		t.Fatal("conflicting receipt event committed")
	}
	assertBackupSnapshot(t, s, time.Unix(1, 0).UTC())
	requireOK(t, s.Close())
	reopened, err := Open(t.Context(), path, options)
	requireOK(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	assertBackupSnapshot(t, reopened, time.Unix(1, 0).UTC())
}

func assertBackupSnapshot(t *testing.T, s *Store, want time.Time) {
	t.Helper()
	stats, err := s.InstallationStats(t.Context())
	requireOK(t, err)
	if stats.LastBackup != want {
		t.Fatal("backup receipt snapshot drifted", stats.LastBackup, want)
	}
}
