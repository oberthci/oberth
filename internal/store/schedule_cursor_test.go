package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestScheduleCursorMigrationBackfillsWithoutChangingLatestOutcomes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "schedule-cursor.db")
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS run_priority_overrides; DROP TABLE IF EXISTS queue_priority_patterns; DELETE FROM schema_migrations WHERE version>=34; ALTER TABLE step_results DROP COLUMN memory_peak_bytes; DELETE FROM schema_migrations WHERE version>=33; ALTER TABLE schedule_fires DROP COLUMN last_fired_at`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_migrations WHERE version=?`, scheduleCursorMigrationVersion); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 8, 24, 3, 5, 0, 0, time.UTC)
	for _, outcome := range []string{"fired", "skipped", "failed", "paused", "refused"} {
		if _, err := s.db.Exec(`INSERT INTO schedule_fires(repo, entry, fired_at, outcome) VALUES(?, ?, ?, ?)`, "alpha", outcome, when.UnixNano(), outcome); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 { // Reopen proves the ledger prevents a duplicate ALTER/backfill.
		s, err = Open(ctx, path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		fires, err := s.ScheduleFires(ctx, "alpha")
		if err != nil || len(fires) != 4 || !fires["fired"].Equal(when) {
			t.Fatalf("successful cursor was not backfilled: %+v, %v", fires, err)
		}
		for _, entry := range []string{"skipped", "failed", "paused"} {
			if cursor, seen := fires[entry]; !seen || !cursor.IsZero() {
				t.Fatalf("%s did not retain pending catch-up: %+v", entry, fires)
			}
		}
		if _, seen := fires["refused"]; seen {
			t.Fatal("refusing an invalid schedule invented a due occurrence")
		}
		outcomes, err := s.ScheduleOutcomes(ctx, "alpha")
		if err != nil || len(outcomes) != 5 {
			t.Fatalf("outcomes changed during migration: %+v, %v", outcomes, err)
		}
		for _, outcome := range outcomes {
			if outcome.Entry != outcome.Outcome || !outcome.FiredAt.Equal(when) {
				t.Fatalf("latest outcome/time changed: %+v", outcome)
			}
		}
		assertFreshSchemaLedger(t, s.db)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
