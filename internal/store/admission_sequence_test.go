package store

import (
	"context"
	"database/sql"
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
	if _, err := raw.ExecContext(ctx, publicationRetryDowngradeSQL+`DROP TABLE IF EXISTS step_resource_usage; DROP INDEX IF EXISTS runs_latest_ref_idx; DROP TABLE IF EXISTS run_priority_overrides; DROP TABLE IF EXISTS queue_priority_patterns; DELETE FROM schema_migrations WHERE version>=34; ALTER TABLE step_results DROP COLUMN memory_peak_bytes; DELETE FROM schema_migrations WHERE version=33; ALTER TABLE schedule_fires DROP COLUMN last_fired_at; DELETE FROM schema_migrations WHERE version=32; DROP TRIGGER run_schedule_entry_immutable; ALTER TABLE runs DROP COLUMN schedule_entry; DELETE FROM schema_migrations WHERE version=31; DROP TRIGGER run_timing_claim; DROP TRIGGER IF EXISTS run_timing_grant; DROP TRIGGER IF EXISTS run_timing_park; DROP TRIGGER run_timing_terminal; DROP TABLE step_timings; DROP TABLE IF EXISTS run_admission_timings; DROP TABLE run_timings;
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

func TestRestartFailureKeepsOriginalAdmissionOrder(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := t.Context()
	a, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/a", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/b", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverOwnerState(ctx); err != nil {
		t.Fatal(err)
	}
	failed, err := s.Run(ctx, a.ID)
	if err != nil || failed.Status != model.RunFailed || failed.QueueSequence != a.QueueSequence || failed.AdmissionSequence != a.AdmissionSequence {
		t.Fatal(failed, err)
	}
	next, err := s.Run(ctx, b.ID)
	if err != nil || next.Status != model.RunFailed || next.Error != model.ServerRestartReason || next.QueueSequence != b.QueueSequence {
		t.Fatal("old queued history not ended", next, err)
	}

}
