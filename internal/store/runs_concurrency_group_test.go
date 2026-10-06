package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// The concurrency group (#658) is recorded on the durable run, where status,
// run_get and run_list read it, and is immutable once recorded: it is a
// statement about the run's exact reviewed bytes.
func TestSetRunConcurrencyGroupRecordsOnceOnActiveRuns(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/state", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if run.ConcurrencyGroup != "" {
		t.Fatalf("a fresh run records group %q before any document was read", run.ConcurrencyGroup)
	}
	// "No group" is not a recording and never an error.
	if err := s.SetRunConcurrencyGroup(ctx, run.ID, ""); err != nil {
		t.Fatalf("empty group: %v", err)
	}
	if err := s.SetRunConcurrencyGroup(ctx, run.ID, "terraform-state"); err != nil {
		t.Fatalf("record group on a queued run: %v", err)
	}
	claimed, err := s.ClaimNextRun(ctx)
	if err != nil || claimed.ID != run.ID || claimed.ConcurrencyGroup != "terraform-state" {
		t.Fatalf("claimed run = %#v, %v; want the recorded group carried", claimed, err)
	}
	// Idempotent for the same value, refused for a different one.
	if err := s.SetRunConcurrencyGroup(ctx, run.ID, "terraform-state"); err != nil {
		t.Fatalf("repeat of the same group: %v", err)
	}
	if err := s.SetRunConcurrencyGroup(ctx, run.ID, "other-group"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("changing a recorded group = %v, want ErrInvalidState", err)
	}
	// Every read path the tools use projects the column.
	stored, err := s.Run(ctx, run.ID)
	if err != nil || stored.ConcurrencyGroup != "terraform-state" {
		t.Fatalf("Run = %#v, %v", stored, err)
	}
	recent, err := s.ListRecentRuns(ctx, model.RunListFilter{})
	if err != nil || len(recent) != 1 || recent[0].ConcurrencyGroup != "terraform-state" {
		t.Fatalf("ListRecentRuns = %#v, %v", recent, err)
	}
	latest, err := s.ListLatestRunsPerRepo(ctx, 5)
	if err != nil || len(latest) != 1 || latest[0].ConcurrencyGroup != "terraform-state" {
		t.Fatalf("ListLatestRunsPerRepo = %#v, %v", latest, err)
	}
	finished, err := s.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunFailed, Phase: "job", Error: "boom"})
	if err != nil || finished.ConcurrencyGroup != "terraform-state" {
		t.Fatalf("terminal run lost its group: %#v, %v", finished, err)
	}
	// A terminal run is left untouched rather than rewritten.
	if err := s.SetRunConcurrencyGroup(ctx, run.ID, "late-group"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("recording a different group on a terminal run = %v, want ErrInvalidState", err)
	}

	other, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/other", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, other.ID, model.RunResult{Status: model.RunInterrupted, Phase: "interrupted"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunConcurrencyGroup(ctx, other.ID, "terraform-state"); err != nil {
		t.Fatalf("terminal ungrouped run: %v", err)
	}
	if value, err := s.Run(ctx, other.ID); err != nil || value.ConcurrencyGroup != "" {
		t.Fatalf("a terminal run gained a group: %#v, %v", value, err)
	}
	if err := s.SetRunConcurrencyGroup(ctx, "missing-run", "terraform-state"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown run = %v, want ErrNotFound", err)
	}
	if err := s.SetRunConcurrencyGroup(ctx, " ", "terraform-state"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank run ID = %v, want ErrInvalid", err)
	}
}

func TestSetRunConcurrencyGroupRefusesNamesOutsideTheGrammar(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/state", strings.Repeat("c", 40)))
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"Terraform", "a_b", "a/b", "a b", strings.Repeat("x", 64), "é"} {
		if err := s.SetRunConcurrencyGroup(ctx, run.ID, group); !errors.Is(err, ErrInvalid) {
			t.Fatalf("group %q = %v, want ErrInvalid", group, err)
		}
	}
	if value, err := s.Run(ctx, run.ID); err != nil || value.ConcurrencyGroup != "" {
		t.Fatalf("a refused name was recorded: %#v, %v", value, err)
	}
}

// The schema CHECK is the backstop behind the Go validation: no write path,
// present or future, can persist a name outside [a-z0-9-]{1,63}.
func TestSchemaRefusesConcurrencyGroupsOutsideTheGrammar(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/state", strings.Repeat("d", 40)))
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"UPPER", "a_b", "a.b", "a/b", " a", strings.Repeat("x", 64), "é"} {
		if _, err := s.db.ExecContext(ctx, `UPDATE runs SET concurrency_group = ? WHERE id = ?`, group, run.ID); err == nil ||
			!strings.Contains(strings.ToLower(err.Error()), "check") {
			t.Fatalf("raw write of %q = %v, want a CHECK constraint failure", group, err)
		}
	}
	for _, group := range []string{"", "a", "-", "terraform-state", strings.Repeat("z", 63)} {
		if _, err := s.db.ExecContext(ctx, `UPDATE runs SET concurrency_group = ? WHERE id = ?`, group, run.ID); err != nil {
			t.Fatalf("raw write of legal %q: %v", group, err)
		}
	}
}

// Migration 16 is additive: a v15 database keeps every run exactly as it was,
// each existing run reads as ungrouped, and the grammar CHECK applies to the
// migrated column as it does to a fresh one.
func TestMigration16AddsAnUngroupedConcurrencyGroupToExistingRuns(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth-v15.sqlite")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	finished, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/finished", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimNextRun(ctx); err != nil || claimed.ID != finished.ID {
		t.Fatalf("claim the run to finish = %#v, %v", claimed, err)
	}
	if _, err := s.FinishRun(ctx, finished.ID, model.RunResult{Status: model.RunFailed, Phase: "job", Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	running, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/running", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimNextRun(ctx); err != nil || claimed.ID != running.ID {
		t.Fatalf("claim the running run = %#v, %v", claimed, err)
	}
	// A durable Job name keeps the run "running" across the reopen, for the
	// scheduler's startup reconciliation, so the upgrade meets a live run.
	if _, err := s.SetRunJobName(ctx, running.ID, "oberth-oberth-running"); err != nil {
		t.Fatal(err)
	}
	queued, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/queued", strings.Repeat("c", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reconstruct the v15 shape: no concurrency_group column, ledger at 15.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `ALTER TABLE runs DROP COLUMN concurrency_group;
DELETE FROM schema_migrations WHERE version >= 16;`); err != nil {
		t.Fatal(err)
	}
	if hasRunsColumn(t, raw, "concurrency_group") {
		t.Fatal("the v15 reconstruction still has the concurrency_group column")
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("v15 -> v16 upgrade: %v", err)
	}
	defer func() {
		if err := migrated.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	var version int
	if err := migrated.db.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 18 {
		t.Fatalf("schema version after upgrade = %d, %v; want 18", version, err)
	}
	for _, want := range []struct {
		id     string
		status model.RunStatus
	}{{finished.ID, model.RunFailed}, {running.ID, model.RunRunning}, {queued.ID, model.RunQueued}} {
		value, err := migrated.Run(ctx, want.id)
		if err != nil || value.Status != want.status || value.ConcurrencyGroup != "" {
			t.Fatalf("run %s after upgrade = status %s group %q, %v; want %s and ungrouped", want.id, value.Status, value.ConcurrencyGroup, err, want.status)
		}
	}
	if _, err := migrated.db.ExecContext(ctx, `UPDATE runs SET concurrency_group = 'Not_A_Group' WHERE id = ?`, queued.ID); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "check") {
		t.Fatalf("raw write of an illegal group on the migrated column = %v, want a CHECK constraint failure", err)
	}
	if err := migrated.SetRunConcurrencyGroup(ctx, queued.ID, "terraform-state"); err != nil {
		t.Fatalf("record a group on a pre-upgrade queued run: %v", err)
	}
	if claimed, err := migrated.ClaimNextRun(ctx); err != nil || claimed.ID != queued.ID || claimed.ConcurrencyGroup != "terraform-state" {
		t.Fatalf("claim after upgrade = %#v, %v", claimed, err)
	}
}

// hasRunsColumn reports whether the runs table currently has the column.
func hasRunsColumn(t *testing.T, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, column string) bool {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT name FROM pragma_table_info('runs')`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return found
}
