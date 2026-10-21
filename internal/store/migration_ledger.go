package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Validate the binary independently of the database: even a fully upgraded
// database must refuse a binary that reuses a shipped migration number.
func validateMigrations(items []migration, latest int) error {
	seen := make(map[int]bool, len(items))
	for _, item := range items {
		if seen[item.version] {
			return fmt.Errorf("%w: duplicate migration version %d in binary", ErrSchemaIncompatible, item.version)
		}
		seen[item.version] = true
	}
	for index, item := range items {
		if item.version != index+1 {
			return fmt.Errorf("%w: binary migration version %d at position %d; expected version %d", ErrSchemaIncompatible, item.version, index, index+1)
		}
		methods := 0
		if item.sql != "" {
			methods++
		}
		if item.apply != nil {
			methods++
		}
		if item.rawApply != nil {
			methods++
		}
		if methods != 1 {
			return fmt.Errorf("%w: migration %d must define exactly one application method", ErrSchemaIncompatible, item.version)
		}
	}
	if len(items) != latest {
		return fmt.Errorf("%w: binary has %d migrations, expected %d", ErrSchemaIncompatible, len(items), latest)
	}
	return nil
}

type migrationQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// The historical ledger already stores every applied version, including legacy
// fixtures. Preserve those records verbatim; never infer missing versions from
// the maximum, since that would conceal an out-of-order deployment.
func readAppliedMigrations(ctx context.Context, q migrationQuerier) (map[int]bool, int, error) {
	if err := validateMigrations(migrations, latestMigrationVersion); err != nil {
		return nil, 0, err
	}
	rows, err := q.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: read migration ledger: %w", ErrSchemaIncompatible, err)
	}
	defer func() { _ = rows.Close() }()
	applied := make(map[int]bool)
	latest := 0
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, 0, fmt.Errorf("read applied migration: %w", err)
		}
		if version > latestMigrationVersion {
			return nil, 0, fmt.Errorf("%w: database=%d binary=%d", ErrSchemaTooNew, version, latestMigrationVersion)
		}
		if version < 1 || applied[version] {
			return nil, 0, fmt.Errorf("%w: invalid or duplicate applied migration version %d", ErrSchemaIncompatible, version)
		}
		applied[version] = true
		latest = version
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read applied migrations: %w", err)
	}
	for version := 1; version <= latest; version++ {
		if !applied[version] {
			return nil, 0, fmt.Errorf("%w: migration ledger missing version %d below applied version %d; out-of-order migrations require operator repair", ErrSchemaIncompatible, version, latest)
		}
	}
	return applied, latest, nil
}
