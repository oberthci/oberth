package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestAdmissionSequenceMigrationBackfillsLegacyRuns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	original := time.Unix(100, 0)
	s, err := Open(ctx, path, Options{Now: func() time.Time { return original }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/legacy", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DROP TRIGGER run_schedule_entry_immutable; ALTER TABLE runs DROP COLUMN schedule_entry; DELETE FROM schema_migrations WHERE version=31; DROP TRIGGER run_timing_claim; DROP TRIGGER run_timing_grant; DROP TRIGGER run_timing_park; DROP TRIGGER run_timing_terminal; DROP TABLE step_timings; DROP TABLE run_admission_timings; DROP TABLE run_timings;
DROP INDEX repository_archive_actions_idx;
DROP INDEX runs_admission_idx;
ALTER TABLE runs DROP COLUMN admission_sequence;
DELETE FROM schema_migrations WHERE version >= ?`, admissionSequenceMigrationVersion); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{Now: func() time.Time { return original.Add(time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	assertFreshSchemaLedger(t, s.db)
	for version := 1; version < admissionSequenceMigrationVersion; version++ {
		var appliedAt int64
		if err := s.db.QueryRowContext(ctx, `SELECT applied_at FROM schema_migrations WHERE version = ?`, version).Scan(&appliedAt); err != nil || appliedAt != unixNano(original) {
			t.Fatalf("migration %d replayed during v27 upgrade: applied_at=%d, error=%v", version, appliedAt, err)
		}
	}
	var appliedAt int64
	if err := s.db.QueryRowContext(ctx, `SELECT applied_at FROM schema_migrations WHERE version = 28`).Scan(&appliedAt); err != nil || appliedAt != unixNano(original.Add(time.Hour)) {
		t.Fatalf("v28 migration application = %d, %v", appliedAt, err)
	}
	got, err := s.Run(ctx, run.ID)
	if err != nil || got.AdmissionSequence != got.QueueSequence {
		t.Fatalf("legacy admission sequence = %+v, %v", got, err)
	}
}

func TestRestartRefireKeepsOriginalAdmissionOrderAndHistoryCursor(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	a, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/a", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNextRun(ctx)
	if err != nil || claimed.ID != a.ID {
		t.Fatalf("claim A = %s, %v", claimed.ID, err)
	}
	b, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/b", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	copy, err := s.RequeueStrandedRun(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if copy.QueueSequence <= b.QueueSequence || copy.AdmissionSequence != a.AdmissionSequence || b.AdmissionSequence != b.QueueSequence {
		t.Fatalf("restart changed FIFO order: A=%+v B=%+v copy=%+v", a.Run, b.Run, copy)
	}
	if _, err := s.RequeueStrandedRun(ctx, a.ID); !errors.Is(err, ErrRequeueIneligible) {
		t.Fatalf("duplicate re-fire = %v, want ineligible", err)
	}
	active, err := s.ListActiveRuns(ctx, model.RunListFilter{})
	if err != nil || len(active) != 2 || active[0].ID != copy.ID || active[1].ID != b.ID {
		t.Fatalf("active order = %v, %v", runIDs(active), err)
	}
	for _, item := range []struct {
		id   string
		want int64
	}{{copy.ID, 1}, {b.ID, 2}} {
		position, err := s.QueuedRunPosition(ctx, item.id)
		if err != nil || position != item.want {
			t.Fatalf("queue position %s = %d, %v; want %d", item.id, position, err, item.want)
		}
	}
	if encoded, err := json.Marshal(copy); err != nil || strings.Contains(string(encoded), "AdmissionSequence") {
		t.Fatalf("public run JSON exposed admission sequence: %s, %v", encoded, err)
	}
	first, err := s.ClaimNextRun(ctx)
	if err != nil || first.ID != copy.ID {
		t.Fatalf("first claim = %s, %v; want re-fired A", first.ID, err)
	}
	second, err := s.ClaimNextRun(ctx)
	if err != nil || second.ID != b.ID {
		t.Fatalf("second claim = %s, %v; want B", second.ID, err)
	}
	newPush, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/a", strings.Repeat("c", 40)))
	if err != nil {
		t.Fatal(err)
	}
	superseded, err := s.Run(ctx, copy.ID)
	if err != nil || superseded.Status != model.RunInterrupted || superseded.SupersededBy != newPush.ID {
		t.Fatalf("new push did not supersede re-fired copy: %+v, %v", superseded, err)
	}
	if _, err := s.FinishRun(ctx, b.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	// History cursors stay on unique queue_sequence even when the default
	// run list presents a queue block ordered by admission_sequence (#1122).
	history, err := s.ListRecentRuns(ctx, model.RunListFilter{Limit: 2, Terminal: true})
	if err != nil || len(history) != 2 || history[0].ID != copy.ID || history[1].ID != b.ID {
		t.Fatalf("history page = %v, %v", runIDs(history), err)
	}
	older, err := s.ListRecentRuns(ctx, model.RunListFilter{Limit: 2, Terminal: true, Before: history[1].QueueSequence})
	if err != nil || len(older) != 1 || older[0].ID != a.ID {
		t.Fatalf("older history page = %v, %v", runIDs(older), err)
	}
}
