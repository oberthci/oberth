package store

import (
	"context"
	"database/sql"

	"github.com/oberthci/oberth/internal/stateport"
)

type schemaMigration struct {
	baseline string
	apply    func(context.Context, *sql.Tx) error
}

type schemaDefinition struct {
	number     int
	baseline   string
	migrations map[int]schemaMigration // Indexed by source version; each step advances one.
}

func currentSchema() schemaDefinition {
	return schemaDefinition{number: Schema, baseline: baseline, migrations: map[int]schemaMigration{}}
}

func (s *Store) prepareSchema(ctx context.Context, target schemaDefinition) error {
	number, err := readBoundProtocol(ctx, s.readers, s.options.Protocol)
	if err != nil {
		return err
	}
	if number > target.number {
		return refuse(newerSchema, nil)
	}
	// Current-schema opens retain read-only verification, even during a write.
	if number == target.number {
		return checkLayout(ctx, s.readers, target.baseline)
	}
	return migrate(ctx, s.writer, s.options.Protocol, target)
}

// One immediate transaction covers all steps, version changes and final validation.
// All steps share the transaction that owns the version change.
func migrate(ctx context.Context, db *sql.DB, protocol stateport.Protocol, target schemaDefinition) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	number, err := readBoundProtocol(ctx, tx, protocol)
	if err != nil {
		return err
	}
	if number > target.number {
		return refuse(newerSchema, nil)
	}
	for ; number < target.number; number++ {
		step, ok := target.migrations[number]
		if !ok {
			return refuse(schema, nil)
		}
		if err := applyMigration(ctx, tx, number, step); err != nil {
			return err
		}
	}
	if err := checkLayout(ctx, tx, target.baseline); err != nil {
		return err
	}
	if err := checkProtocol(ctx, tx, stateport.Options{Protocol: protocol, Schema: target.number}); err != nil {
		return err
	}
	return databaseError(tx.Commit())
}

func applyMigration(ctx context.Context, tx *sql.Tx, number int, step schemaMigration) error {
	if err := checkLayout(ctx, tx, step.baseline); err != nil {
		return err
	}
	if err := step.apply(ctx, tx); err != nil {
		return databaseError(err)
	}
	// The ordinary write fence is removed and restored within the same transaction.
	var trigger string
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name='protocol_immutable'`).Scan(&trigger); err != nil {
		return databaseError(err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TRIGGER protocol_immutable`); err != nil {
		return databaseError(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE protocol SET schema_number=? WHERE singleton=1`, number+1); err != nil {
		return databaseError(err)
	}
	_, err := tx.ExecContext(ctx, trigger)
	return databaseError(err)
}
