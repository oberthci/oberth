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

// TestMigrationLiveLineageAppliesRebuildAfterRecordedV10 reproduces the
// production upgrade lineage: v0.13.25/v0.13.26 shipped migration v10 as a
// ledger-only `SELECT 1`, so every deployed database has version 10 RECORDED
// with none of the canonical-persistence work applied. The migration runner
// skips each version already recorded in the ledger, which means the
// canonical-persistence rebuild MUST live at a version those databases have
// not recorded (v11/v12) — replacing v10's body would silently never run on
// exactly the databases that need it, forking the live schema from every
// fresh one (empirically reproduced during the #245 G3 review: same-name
// registration kept failing UNIQUE(name) on the live lineage while all
// fresh-DB tests passed).
//
// The test rebuilds that lineage byte-for-byte — old repositories shape,
// bare schedule_fires and secret_access rows, ledger at versions 1..10 —
// then reopens through the production path and proves v11+v12 deliver:
// compound unique admits a same-name repo, schedule fires are qualified,
// grants are qualified, and foreign keys stay clean.
func TestMigrationLiveLineageAppliesRebuildAfterRecordedV10(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 26, 6, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth-live-lineage.sqlite")
	ctx := context.Background()

	// Step 1: create a fully-migrated database and register the live shape:
	// two upstreams and one repository, exactly one bare schedule fire and
	// one bare grant the way the pre-G3 deployment persisted them.
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	up1, err := s.RegisterUpstream(ctx, "admin@test", model.UpstreamSpec{
		Name: "codeberg", BaseURL: "ssh://git@codeberg.org/acme", Kind: "ssh",
	})
	if err != nil {
		t.Fatal(err)
	}
	up2, err := s.RegisterUpstream(ctx, "admin@test", model.UpstreamSpec{
		Name: "skipops", BaseURL: "ssh://git@codeberg.org/skipops", Kind: "ssh",
	})
	if err != nil {
		t.Fatal(err)
	}
	terraform, err := s.RegisterRepository(ctx, "admin@test", model.RepositorySpec{
		Name: "terraform", UpstreamID: up1.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A run recorded before the upgrade, so the additive runs migrations
	// (v16 concurrency group) are proven against an existing row.
	liveRun, err := s.EnqueueRun(ctx, testRunSpec(terraform.ID, "fix-live", strings.Repeat("e", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Step 2: surgery back to the exact live (v0.13.26) schema state:
	// single-column UNIQUE(name) on repositories, bare persisted keys, and
	// the migration ledger ending at version 10.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `
PRAGMA foreign_keys = OFF;
CREATE TABLE repositories_live (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    upstream_id INTEGER NOT NULL REFERENCES upstreams(id),
    default_branch TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
INSERT INTO repositories_live SELECT id, name, upstream_id, default_branch, created_at, updated_at FROM repositories;
DROP TABLE repositories;
ALTER TABLE repositories_live RENAME TO repositories;
DELETE FROM schedule_fires;
INSERT INTO schedule_fires(repo, entry, fired_at, outcome) VALUES('terraform', 'nightly', 1000, 'fired');
DELETE FROM secret_access;
INSERT INTO secret_access(repo, step, secret, approved_by, approved_at) VALUES('terraform', '*', 'oberth/data/release/cosign-secret', 'admin@test', 1000);
DROP TABLE vm_suite_resources;
DROP TABLE vm_suite_executions;
DROP TABLE vm_capacity_slots;
DROP TABLE vm_executions;
DROP TRIGGER run_schedule_entry_immutable; ALTER TABLE runs DROP COLUMN schedule_entry; DELETE FROM schema_migrations WHERE version=31; DROP TRIGGER run_timing_claim; DROP TRIGGER run_timing_grant; DROP TRIGGER run_timing_park; DROP TRIGGER run_timing_terminal; DROP TABLE step_timings; DROP TABLE run_admission_timings; DROP TABLE run_timings;
DROP INDEX runs_admission_idx; ALTER TABLE runs DROP COLUMN admission_sequence;
ALTER TABLE runs DROP COLUMN concurrency_group;
DROP TABLE IF EXISTS grant_declarations;
DROP TRIGGER IF EXISTS promotions_guard_update;
DROP TRIGGER IF EXISTS promotion_grants_no_delete;
DROP TRIGGER IF EXISTS promotion_grants_guard_update;
DROP TABLE IF EXISTS promotion_grants;
ALTER TABLE promotions DROP COLUMN grant_id;
CREATE TRIGGER promotions_guard_update
BEFORE UPDATE ON promotions
WHEN NOT (
    OLD.status = 'pending'
    AND NEW.sequence = OLD.sequence AND NEW.id = OLD.id AND NEW.repo_id = OLD.repo_id
    AND NEW.source_branch = OLD.source_branch AND NEW.source_sha = OLD.source_sha
    AND NEW.target_ref = OLD.target_ref AND NEW.actor = OLD.actor AND NEW.created_at = OLD.created_at
    AND (
        (NEW.status = 'pending' AND NEW.error = OLD.error
         AND ((NEW.previous_sha = OLD.previous_sha AND NEW.result_sha = OLD.result_sha)
              OR (OLD.previous_sha = '' AND OLD.result_sha = '' AND NEW.previous_sha != '' AND NEW.result_sha != ''))
         AND (NEW.run_id = OLD.run_id OR OLD.run_id = ''))
        OR (NEW.status IN ('passed', 'failed', 'interrupted')
            AND NEW.previous_sha = OLD.previous_sha AND NEW.result_sha = OLD.result_sha
            AND (NEW.run_id = OLD.run_id OR OLD.run_id = ''))
    )
)
BEGIN SELECT RAISE(ABORT, 'promotion transition is immutable'); END;
DROP TABLE kubevirt_grants; DROP TABLE uplink_scopes; ALTER TABLE issues DROP COLUMN scope_upstream_id;
ALTER TABLE step_results DROP COLUMN termination_json;
DELETE FROM schema_migrations WHERE version > 10;
PRAGMA foreign_keys = ON;`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	// Step 3: reopen through the production path — the live upgrade.
	s2, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("live-lineage upgrade failed: %v", err)
	}
	defer func() {
		if err := s2.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()

	assertFreshSchemaLedger(t, s2.db)
	for version := 1; version <= 10; version++ {
		var appliedAt int64
		if err := s2.db.QueryRowContext(ctx, `SELECT applied_at FROM schema_migrations WHERE version = ?`, version).Scan(&appliedAt); err != nil {
			t.Fatal(err)
		}
		if appliedAt != unixNano(now) {
			t.Fatalf("legacy migration %d record changed: %d", version, appliedAt)
		}
	}

	// The rebuild must have replaced the single-column unique.
	var createSQL string
	if err := s2.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_schema WHERE type='table' AND name='repositories'`).Scan(&createSQL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(createSQL, "UNIQUE(upstream_id, name)") {
		t.Fatalf("compound unique missing after live-lineage upgrade; repositories DDL:\n%s", createSQL)
	}

	// Same-name repo under the second upstream must now be admitted.
	repo2, err := s2.RegisterRepository(ctx, "admin@test", model.RepositorySpec{
		Name: "terraform", UpstreamID: up2.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatalf("same-name repo still refused after live-lineage upgrade: %v", err)
	}
	if repo2.UpstreamID != up2.ID {
		t.Fatalf("second terraform upstream = %d, want %d", repo2.UpstreamID, up2.ID)
	}

	// Bare-name lookup must now be ambiguous.
	if _, err := s2.RepositoryByName(ctx, "terraform"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("bare-name lookup = %v, want ErrAmbiguous", err)
	}

	// The schedule fire key must be qualified by the v11 data migration.
	var scheduleRepo string
	if err := s2.db.QueryRowContext(ctx, `SELECT repo FROM schedule_fires`).Scan(&scheduleRepo); err != nil {
		t.Fatal(err)
	}
	if scheduleRepo != "codeberg/acme/terraform" {
		t.Fatalf("schedule_fires key = %q, want codeberg/acme/terraform", scheduleRepo)
	}

	// The grant key must be qualified by the v12 data migration.
	var grantRepo string
	if err := s2.db.QueryRowContext(ctx, `SELECT repo FROM secret_access`).Scan(&grantRepo); err != nil {
		t.Fatal(err)
	}
	if grantRepo != "codeberg/acme/terraform" {
		t.Fatalf("secret_access key = %q, want codeberg/acme/terraform", grantRepo)
	}

	// v16 (#658) lands on the live lineage as well: the column exists and the
	// pre-upgrade run reads as ungrouped rather than failing to scan.
	if !hasRunsColumn(t, s2.db, "concurrency_group") {
		t.Fatal("runs.concurrency_group missing after live-lineage upgrade")
	}
	if value, err := s2.Run(ctx, liveRun.ID); err != nil || value.ConcurrencyGroup != "" || value.Status != model.RunQueued {
		t.Fatalf("pre-upgrade run after live-lineage upgrade = %#v, %v", value, err)
	}

	// Foreign keys stay clean through the rebuild.
	fkRows, err := s2.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fkRows.Close() }()
	if fkRows.Next() {
		t.Fatal("foreign_key_check reported violations after live-lineage upgrade")
	}
	if err := fkRows.Err(); err != nil {
		t.Fatal(err)
	}
}
