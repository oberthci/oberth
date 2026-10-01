package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// connectionLevelMigration runs outside the normal migration transaction on the
// raw *sql.DB. It is used for migrations that require connection-level PRAGMA
// changes (e.g., PRAGMA foreign_keys = OFF for FK-safe table rebuilds).
type connectionLevelMigration func(ctx context.Context, db *sql.DB, now func() time.Time) error

// v11ScheduleFireRow is a row from the schedule_fires table, used during
// the v11 migration.
type v11ScheduleFireRow struct {
	repo, entry, outcome string
	firedAt              int64
}

// migrateV11CanonicalPersistence implements the G3 canonical persistence
// migration (#245). It rebuilds the repositories table to replace the
// single-column UNIQUE(name) with the compound UNIQUE(upstream_id, name),
// allowing same-name repos under different upstreams.
//
// It also migrates schedule_fires from bare repo names to qualified
// upstream_name/org/repo_name keys.
//
// This migration must run outside the normal migration transaction because
// PRAGMA foreign_keys can only be changed when no transaction is active.
func migrateV11CanonicalPersistence(ctx context.Context, db *sql.DB, now func() time.Time) error {
	// Idempotency check: if the migration already ran but the version
	// recording failed, the table already has the compound unique. Inspect
	// the CREATE statement to detect this.
	var createSQL string
	if err := db.QueryRowContext(ctx, `
		SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = 'repositories'
	`).Scan(&createSQL); err != nil {
		return fmt.Errorf("migration v11: inspect repositories schema: %w", err)
	}
	if strings.Contains(createSQL, "UNIQUE(upstream_id, name)") {
		return nil // already migrated
	}

	// Step 1: Build the upstream lookup for schedule_fires migration.
	// This must happen before we touch the repositories table.
	qualifiedNames := make(map[string]string) // bare name -> "upstream/org/repo"
	rows, err := db.QueryContext(ctx, `
		SELECT r.name, u.name, u.base_url
		FROM repositories r
		JOIN upstreams u ON u.id = r.upstream_id`)
	if err != nil {
		return fmt.Errorf("migration v11: read repo-upstream mapping: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var repoName, upstreamName, baseURL string
		if err := rows.Scan(&repoName, &upstreamName, &baseURL); err != nil {
			return fmt.Errorf("migration v11: scan repo-upstream row: %w", err)
		}
		org := v11OrgFromBaseURL(baseURL)
		if org == "" {
			org = upstreamName
		}
		qualifiedNames[repoName] = upstreamName + "/" + org + "/" + repoName
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migration v11: read repo-upstream mapping: %w", err)
	}

	// Step 2: Disable foreign keys for the FK-safe table rebuild.
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("migration v11: disable foreign keys: %w", err)
	}
	// Ensure foreign keys are re-enabled even on error.
	defer func() { _, _ = db.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()

	// Step 3: Rebuild repositories + schedule_fires inside one transaction.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration v11: begin rebuild: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 3a: Rebuild repositories with compound UNIQUE(upstream_id, name).
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE repositories_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    upstream_id INTEGER NOT NULL REFERENCES upstreams(id),
    default_branch TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(upstream_id, name)
)`); err != nil {
		return fmt.Errorf("migration v11: create repositories_new: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO repositories_new(id, name, upstream_id, default_branch, created_at, updated_at)
SELECT id, name, upstream_id, default_branch, created_at, updated_at FROM repositories`); err != nil {
		return fmt.Errorf("migration v11: copy repositories: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE repositories`); err != nil {
		return fmt.Errorf("migration v11: drop old repositories: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE repositories_new RENAME TO repositories`); err != nil {
		return fmt.Errorf("migration v11: rename repositories_new: %w", err)
	}

	// 3b: Rebuild schedule_fires with qualified repo names.
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE schedule_fires_new (
    repo TEXT NOT NULL,
    entry TEXT NOT NULL,
    fired_at INTEGER NOT NULL,
    outcome TEXT NOT NULL,
    PRIMARY KEY (repo, entry)
) WITHOUT ROWID`); err != nil {
		return fmt.Errorf("migration v11: create schedule_fires_new: %w", err)
	}
	sfData, err := v11ReadScheduleFires(ctx, tx)
	if err != nil {
		return err
	}
	for _, r := range sfData {
		qualified, ok := qualifiedNames[r.repo]
		if !ok {
			// Unknown repo in schedule_fires -- orphaned data, skip.
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO schedule_fires_new(repo, entry, fired_at, outcome) VALUES(?, ?, ?, ?)`,
			qualified, r.entry, r.firedAt, r.outcome); err != nil {
			return fmt.Errorf("migration v11: migrate schedule_fire %q: %w", r.repo, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE schedule_fires`); err != nil {
		return fmt.Errorf("migration v11: drop old schedule_fires: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE schedule_fires_new RENAME TO schedule_fires`); err != nil {
		return fmt.Errorf("migration v11: rename schedule_fires_new: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration v11: commit rebuild: %w", err)
	}

	// Step 4: Re-enable foreign keys and verify.
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("migration v11: re-enable foreign keys: %w", err)
	}
	if err := v11ForeignKeyCheck(ctx, db); err != nil {
		return err
	}

	return nil
}

func v11ReadScheduleFires(ctx context.Context, tx *sql.Tx) ([]v11ScheduleFireRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT repo, entry, fired_at, outcome FROM schedule_fires`)
	if err != nil {
		return nil, fmt.Errorf("migration v11: read schedule_fires: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var data []v11ScheduleFireRow
	for rows.Next() {
		var r v11ScheduleFireRow
		if err := rows.Scan(&r.repo, &r.entry, &r.firedAt, &r.outcome); err != nil {
			return nil, fmt.Errorf("migration v11: scan schedule_fire: %w", err)
		}
		data = append(data, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migration v11: iterate schedule_fires: %w", err)
	}
	return data, nil
}

func v11ForeignKeyCheck(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("migration v11: foreign key check query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		return fmt.Errorf("migration v11: foreign key check found violations after rebuild")
	}
	return rows.Err()
}

// v11OrgFromBaseURL extracts the organization identity from an upstream's
// base URL. This is the same derivation as model.Upstream.Org() but operates
// on a raw string to avoid constructing a model object in the migration.
func v11OrgFromBaseURL(baseURL string) string {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ""
	}
	if filepath.IsAbs(base) {
		return filepath.Base(base)
	}
	parts := strings.Split(base, "/")
	return parts[len(parts)-1]
}
