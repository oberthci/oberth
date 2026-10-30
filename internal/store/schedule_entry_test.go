package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestScheduleEntriesAreIndependentAndSurviveRetryRecovery(t *testing.T) {
	now := time.Date(2026, 10, 24, 1, 30, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	repo := createRepo(t, s)
	spec := model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("a", 40), Actor: "oberth:scheduler", Trigger: "schedule", ScheduleEntry: "nightly"}
	nightly, err := s.EnqueueRun(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.ScheduleEntry = "weekly"
	weekly, err := s.EnqueueRun(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := s.Run(ctx, nightly.ID)
	if err != nil || retained.Status != model.RunQueued || retained.ScheduleEntry != "nightly" {
		t.Fatalf("entries collapsed: %+v %v", retained, err)
	}
	spec.Trigger, spec.ScheduleEntry = "branch", ""
	branch, err := s.EnqueueRun(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	retained, err = s.Run(ctx, weekly.ID)
	if err != nil || retained.Status != model.RunQueued || retained.ScheduleEntry != "weekly" {
		t.Fatalf("push superseded independent schedule: %+v %v", retained, err)
	}
	if branch.ScheduleEntry != "" {
		t.Fatal("branch inherited entry")
	}
	// Retry must consult the original run, even when another entry of the
	// same branch has newer queued work and the schedule file has changed.
	if _, err = s.FinishRun(ctx, nightly.ID, model.RunResult{Status: model.RunInterrupted}); err != nil {
		t.Fatal(err)
	}
	retried, err := s.RetryRun(ctx, nightly.ID, "admin")
	if err != nil || retried.ScheduleEntry != "nightly" {
		t.Fatalf("retry: %+v %v", retried, err)
	}
	if _, err = s.FinishRun(ctx, retried.ID, model.RunResult{Status: model.RunFailed, Phase: "test"}); err != nil {
		t.Fatal(err)
	}
	rerun, err := s.RerunRun(ctx, model.RunRerunRequest{RunID: retried.ID, Actor: "admin"})
	if err != nil || rerun.Enqueued.ScheduleEntry != "nightly" {
		t.Fatalf("rerun: %+v %v", rerun, err)
	}
	claimed, err := s.ClaimNextRun(ctx)
	if err != nil || claimed.ID != weekly.ID {
		t.Fatalf("FIFO: %+v %v", claimed, err)
	}
	if _, err = s.SetRunJobName(ctx, weekly.ID, "lost-schedule-workflow"); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.RequeueStrandedRun(ctx, weekly.ID)
	if err != nil || recovered.ScheduleEntry != "weekly" {
		t.Fatalf("restart recovery: %+v %v", recovered, err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE runs SET schedule_entry='forged' WHERE id=?`, recovered.ID); err == nil {
		t.Fatal("durable entry mutated")
	}
	for _, trigger := range []string{"branch", "tag", "promotion"} {
		invalid := spec
		invalid.Trigger = trigger
		invalid.ScheduleEntry = "nightly"
		if _, err = s.EnqueueRun(ctx, invalid); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s forged identity: %v", trigger, err)
		}
	}
}

func TestScheduleEntryMigrationPreservesLegacyIdentityAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	legacy, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("a", 40), Actor: "oberth:scheduler", Trigger: "schedule"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DROP TABLE IF EXISTS run_priority_overrides; DROP TABLE IF EXISTS queue_priority_patterns; DELETE FROM schema_migrations WHERE version>=34; ALTER TABLE step_results DROP COLUMN memory_peak_bytes; DELETE FROM schema_migrations WHERE version>=33; ALTER TABLE schedule_fires DROP COLUMN last_fired_at; DELETE FROM schema_migrations WHERE version=32; DROP TRIGGER run_schedule_entry_immutable; ALTER TABLE runs DROP COLUMN schedule_entry; DELETE FROM schema_migrations WHERE version=31`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Run(ctx, legacy.ID)
	if err != nil || !reflect.DeepEqual(got, legacy.Run) {
		t.Fatalf("legacy run changed: %+v vs %+v (%v)", got, legacy.Run, err)
	}
	// Unknown old entries stay unknown; a current schedule file cannot
	// backfill a guess into historical run identity.
	if got.ScheduleEntry != "" {
		t.Fatal("legacy identity invented")
	}
	assertFreshSchemaLedger(t, s.db)
}
