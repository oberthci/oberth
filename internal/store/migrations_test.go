package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

const migrationTable = `CREATE TABLE migration_marker(value TEXT NOT NULL);`
const migrationColumn = `ALTER TABLE migration_marker ADD COLUMN complete INTEGER NOT NULL DEFAULT 1;`

func TestCurrentSchemaOpenPreservesFixtureBytes(t *testing.T) {
	t.Parallel()
	path, fixture := migrationFixturePath(t, 6)
	before, err := os.ReadFile(path)
	requireOK(t, err)
	s, err := Open(t.Context(), path, testOptions())
	requireOK(t, err)
	assertMigrationRecords(t, s, fixture)
	requireOK(t, s.Close())
	after, err := os.ReadFile(path)
	requireOK(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("current-schema open rewrote the database")
	}
}

func TestCurrentSchemaOpenWhileWriterIsBusy(t *testing.T) {
	t.Parallel()
	path, fixture := migrationFixturePath(t, 6)
	db := pool(path, "rw", testOptions(), 1)
	defer func() { requireOK(t, db.Close()) }()
	tx, err := db.BeginTx(t.Context(), nil)
	requireOK(t, err)
	defer func() { _ = tx.Rollback() }()
	s, err := Open(t.Context(), path, testOptions())
	requireOK(t, err)
	assertMigrationRecords(t, s, fixture)
	requireOK(t, s.Close())
}

func testMigrationTarget() schemaDefinition {
	return schemaDefinition{number: 8, baseline: baseline + migrationTable + migrationColumn, migrations: map[int]schemaMigration{
		6: {baseline: baseline, apply: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, migrationTable+`INSERT INTO migration_marker VALUES('preserved');`)
			return err
		}},
		7: {baseline: baseline + migrationTable, apply: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, migrationColumn)
			return err
		}},
	}}
}

func TestForwardMigrationOpensPreviousCodeFixture(t *testing.T) {
	t.Parallel()
	path, fixture := migrationFixturePath(t, 6)
	for range 2 {
		s, err := open(t.Context(), path, testOptions(), testMigrationTarget())
		requireOK(t, err)
		assertMigrationRecords(t, s, fixture)
		version, protocol, err := s.StateMetadata(t.Context())
		requireOK(t, err)
		if version != 8 || protocol != testOptions().Protocol {
			t.Fatal("migration lost version or binding", version, protocol)
		}
		var value string
		var complete int
		requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT value,complete FROM migration_marker`).Scan(&value, &complete))
		if value != "preserved" || complete != 1 {
			t.Fatal("migration did not run both steps")
		}
		if _, err := s.writer.ExecContext(t.Context(), `UPDATE protocol SET schema_number=9`); err == nil {
			t.Fatal("migration removed the protocol write fence")
		}
		requireOK(t, s.Close())
	}
}

func TestForwardMigrationRollsBackWholeChain(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"step", "gap", "final-layout", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			path, fixture := migrationFixturePath(t, 6)
			target := testMigrationTarget()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch failure {
			case "step", "canceled":
				step := target.migrations[7]
				target.migrations[7] = schemaMigration{baseline: step.baseline, apply: func(ctx context.Context, tx *sql.Tx) error {
					if err := step.apply(ctx, tx); err != nil {
						return err
					}
					if failure == "canceled" {
						cancel()
						return ctx.Err()
					}
					return errors.New("injected second step failure")
				}}
			case "gap":
				delete(target.migrations, 7)
			case "final-layout":
				target.baseline = baseline
			}
			if s, err := open(ctx, path, testOptions(), target); err == nil {
				requireOK(t, s.Close())
				t.Fatal("incomplete migration committed")
			}
			s, err := Open(t.Context(), path, testOptions())
			requireOK(t, err)
			assertMigrationRecords(t, s, fixture)
			version, _, err := s.StateMetadata(t.Context())
			requireOK(t, err)
			if version != 6 {
				t.Fatal("failed migration changed the version", version)
			}
			requireOK(t, checkSchema(t.Context(), s.readers))
			requireOK(t, s.Close())
		})
	}
}

func TestForwardMigrationChecksSourceAndBindingBeforeStep(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"source-layout", "foreign", "newer"} {
		t.Run(failure, func(t *testing.T) {
			path, _ := migrationFixturePath(t, 6)
			target := testMigrationTarget()
			options := testOptions()
			switch failure {
			case "source-layout":
				db := pool(path, "rw", options, 1)
				execute(t, db, `CREATE TABLE unexpected(value TEXT)`)
				requireOK(t, db.Close())
			case "foreign":
				options.Protocol.AuditDomain += "-foreign"
			case "newer":
				target.number = 5
			}
			target.migrations[6] = schemaMigration{baseline: baseline, apply: func(context.Context, *sql.Tx) error {
				t.Error("migration ran before source validation")
				return nil
			}}
			_, err := open(t.Context(), path, options, target)
			requireCode(t, err, publicerr.SchemaRefused)
		})
	}
}

func TestForwardMigrationRetainsProtocolBinding(t *testing.T) {
	for _, field := range []string{"identity", "domain"} {
		t.Run(field, func(t *testing.T) {
			path, fixture := migrationFixturePath(t, 6)
			target := testMigrationTarget()
			step := target.migrations[7]
			target.migrations[7] = schemaMigration{baseline: step.baseline, apply: func(ctx context.Context, tx *sql.Tx) error {
				if err := step.apply(ctx, tx); err != nil {
					return err
				}
				var trigger string
				if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name='protocol_immutable'`).Scan(&trigger); err != nil {
					return err
				}
				_, err := tx.ExecContext(ctx, `DROP TRIGGER protocol_immutable; UPDATE protocol SET `+field+`='changed';`+trigger)
				return err
			}}
			s, err := open(t.Context(), path, testOptions(), target)
			if s != nil {
				requireOK(t, s.Close())
			}
			if err == nil {
				t.Fatal("migration changed installation binding")
			}
			requireCode(t, err, publicerr.SchemaRefused)
			s, err = Open(t.Context(), path, testOptions())
			requireOK(t, err)
			assertMigrationRecords(t, s, fixture)
			requireOK(t, s.Close())
		})
	}
}

func TestConcurrentOpenNeverRepeatsMigration(t *testing.T) {
	path, fixture := migrationFixturePath(t, 6)
	target := testMigrationTarget()
	started, release := make(chan struct{}), make(chan struct{})
	step := target.migrations[6]
	target.migrations[6] = schemaMigration{baseline: step.baseline, apply: func(ctx context.Context, tx *sql.Tx) error {
		close(started)
		<-release
		return step.apply(ctx, tx)
	}}
	done := make(chan error, 1)
	go func() {
		s, err := open(t.Context(), path, testOptions(), target)
		if err == nil {
			err = s.Close()
		}
		done <- err
	}()
	<-started
	_, err := open(t.Context(), path, testOptions(), testMigrationTarget())
	close(release)
	requireCode(t, err, publicerr.Unavailable)
	requireOK(t, <-done)
	s, err := open(t.Context(), path, testOptions(), testMigrationTarget())
	requireOK(t, err)
	assertMigrationRecords(t, s, fixture)
	requireOK(t, s.Close())
}
