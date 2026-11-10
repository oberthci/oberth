package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

type migrationQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func migrationLedgerHasIDs(ctx context.Context, q migrationQuerier) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info('schema_migrations') WHERE name='id'`)
	if err != nil {
		return false, fmt.Errorf("%w: inspect migration ledger: %w", ErrSchemaIncompatible, err)
	}
	defer func() { _ = rows.Close() }()
	hasIDs := rows.Next()
	return hasIDs, rows.Err()
}

// Keep the historical ledger and its timestamps. The integer PK becomes a local
// receipt ordinal after 55; IDs, not ordinals or a high-water mark, select work.
// An older binary still refuses the larger ordinal before making any writes.
func ensureMigrationIDs(ctx context.Context, tx *sql.Tx) error {
	hasIDs, err := migrationLedgerHasIDs(ctx, tx)
	if err != nil {
		return err
	}
	if !hasIDs {
		if _, err = tx.ExecContext(ctx, `ALTER TABLE schema_migrations ADD COLUMN id TEXT`); err != nil {
			return fmt.Errorf("add migration IDs: %w", err)
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE schema_migrations SET id=CAST(version AS TEXT) WHERE id IS NULL AND version<=?`, lastIntegerMigrationVersion); err != nil {
		return fmt.Errorf("record legacy migration IDs: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS schema_migrations_id_unique ON schema_migrations(id)`); err != nil {
		return fmt.Errorf("index migration IDs: %w", err)
	}
	return nil
}

func readAppliedMigrations(ctx context.Context, q migrationQuerier) (map[string]bool, int, error) {
	if migrationDefinitionError != nil {
		return nil, 0, migrationDefinitionError
	}
	return readAppliedMigrationsWith(ctx, q, migrations)
}

func readAppliedMigrationsWith(ctx context.Context, q migrationQuerier, items []migration) (map[string]bool, int, error) {
	if err := validateMigrations(items, len(items)); err != nil {
		return nil, 0, err
	}
	known := make(map[string]bool, len(items))
	for _, item := range items {
		known[item.ID()] = true
	}
	hasIDs, err := migrationLedgerHasIDs(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	query := `SELECT version, NULL FROM schema_migrations ORDER BY version`
	if hasIDs {
		query = `SELECT version, id FROM schema_migrations ORDER BY version`
	}
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: read migration ledger: %w", ErrSchemaIncompatible, err)
	}
	defer func() { _ = rows.Close() }()
	applied := make(map[string]bool)
	receipts := make(map[int]bool)
	latest := 0
	for rows.Next() {
		var version int
		var recordedID sql.NullString
		if err := rows.Scan(&version, &recordedID); err != nil {
			return nil, 0, fmt.Errorf("read applied migration: %w", err)
		}
		id := recordedID.String
		if !recordedID.Valid {
			// A pre-ID database can only contain the frozen integer lineage.
			if version > lastIntegerMigrationVersion {
				return nil, 0, fmt.Errorf("%w: database=%d binary=%d; this binary is older than the database: run the release that wrote it or newer, or roll back with oberth upgrade --rollback (see docs/backup-restore.md)", ErrSchemaTooNew, version, len(items))
			}
			id = strconv.Itoa(version)
		}
		if version < 1 || receipts[version] || applied[id] || id == "" {
			return nil, 0, fmt.Errorf("%w: invalid or duplicate applied migration ID %q at receipt %d", ErrSchemaIncompatible, id, version)
		}
		if !known[id] {
			return nil, 0, fmt.Errorf("%w: unknown applied migration ID %q; this binary is older than the database (see docs/backup-restore.md)", ErrSchemaTooNew, id)
		}
		if (version <= lastIntegerMigrationVersion && id != strconv.Itoa(version)) || (version > lastIntegerMigrationVersion && !validTimestampMigrationID(id)) {
			return nil, 0, fmt.Errorf("%w: migration ID %q at invalid receipt %d", ErrSchemaIncompatible, id, version)
		}
		applied[id] = true
		receipts[version] = true
		latest = version
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read applied migrations: %w", err)
	}
	// Gaps in the receipt sequence remain corruption. A missing timestamped ID
	// is ordinary pending work even if a lexically later ID has already run.
	for version := 1; version <= latest; version++ {
		if !receipts[version] {
			return nil, 0, fmt.Errorf("%w: migration ledger missing version %d below applied version %d; out-of-order migrations require operator repair", ErrSchemaIncompatible, version, latest)
		}
	}
	return applied, latest, nil
}
