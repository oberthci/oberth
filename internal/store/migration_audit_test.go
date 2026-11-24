package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestMigrationRejectsAuditDamageBeforeCommit(t *testing.T) {
	for _, field := range []string{"hash", "previous", "head"} {
		t.Run(field, func(t *testing.T) {
			path, fixture := migrationFixturePath(t, 6)
			original, err := Open(t.Context(), path, testOptions())
			requireOK(t, err)
			head := independentHead(t, original.readers)
			requireOK(t, original.Close())
			target := testMigrationTarget()
			step := target.migrations[7]
			target.migrations[7] = schemaMigration{baseline: step.baseline, apply: func(ctx context.Context, tx *sql.Tx) error {
				if err := step.apply(ctx, tx); err != nil {
					return err
				}
				return corruptMigrationAudit(ctx, tx, field)
			}}
			s, err := open(t.Context(), path, testOptions(), target)
			if s != nil {
				requireOK(t, s.Close())
			}
			if err == nil {
				t.Fatal("migration committed a broken audit chain")
			}
			requireCode(t, err, publicerr.SchemaRefused)
			s, err = Open(t.Context(), path, testOptions())
			requireOK(t, err)
			defer func() { requireOK(t, s.Close()) }()
			assertMigrationRecords(t, s, fixture)
			if independentHead(t, s.readers) != head {
				t.Fatal("failed migration changed the audit head")
			}
			requireOK(t, checkSchema(t.Context(), s.readers))
		})
	}
}

func corruptMigrationAudit(ctx context.Context, tx *sql.Tx, field string) error {
	if field == "head" {
		_, err := tx.ExecContext(ctx, `UPDATE audit_head SET hash='broken'`)
		return err
	}
	var trigger string
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name='audit_immutable'`).Scan(&trigger); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TRIGGER audit_immutable`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE audit SET `+field+`='broken' WHERE id=(SELECT min(id) FROM audit)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, trigger)
	return err
}
