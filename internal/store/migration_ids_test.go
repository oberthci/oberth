package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestMigrationIDsUnknownAppliedIDRefusesBeforeRecovery(t *testing.T) {
	for name, open := range map[string]func(context.Context, string, Options) (*Store, error){"owner": Open, "admin": OpenAdminClient, "current": OpenCurrent, "inspection": InspectCurrent} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unknown.sqlite")
			s, err := OpenAdminClient(t.Context(), path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			// Future branch IDs must be checked even when their receipt count is no
			// higher than this binary's. Numeric MAX(version) cannot detect this.
			var hasID int
			if err = s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('schema_migrations') WHERE name='id'`).Scan(&hasID); err != nil {
				t.Fatal(err)
			}
			if hasID == 0 {
				if _, err = s.db.Exec(`ALTER TABLE schema_migrations ADD COLUMN id TEXT`); err != nil {
					t.Fatal(err)
				}
			}
			unknown := "20991231235959-future-branch"
			if _, err = s.db.Exec(`UPDATE schema_migrations SET id=? WHERE version=(SELECT max(version) FROM schema_migrations)`, unknown); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			before := readAuthoritativeDatabaseFiles(t, path)
			opened, err := open(t.Context(), path, Options{})
			if opened != nil {
				_ = opened.Close()
			}
			if !errors.Is(err, ErrSchemaTooNew) || !strings.Contains(err.Error(), unknown) {
				t.Fatalf("unknown ID accepted or concealed: %v", err)
			}
			after := readAuthoritativeDatabaseFiles(t, path)
			assertDatabaseFilesEqual(t, after, before, "unknown migration ID refusal")
		})
	}
}

func TestMigrationIDsCurrentLedgerRecordsEveryID(t *testing.T) {
	s, err := OpenAdminClient(t.Context(), filepath.Join(t.TempDir(), "ids.sqlite"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.db.Query(`SELECT version,id FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var version int
		var id sql.NullString
		if err = rows.Scan(&version, &id); err != nil {
			t.Fatal(err)
		}
		if !id.Valid || id.String == "" {
			t.Fatal("migration has no ID", version)
		}
		n++
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n <= 55 {
		t.Fatal("ID-ledger transition did not follow the last integer migration")
	}
}

// Separate catalogs model independent builds: neither branch reserves a number
// or edits a registry. The SQL deliberately isn't idempotent, detecting replay.
func TestMigrationIDsMergeEitherOrderAndUpgradeIntegerLedger(t *testing.T) {
	a := migration{id: "20990101000001-add-alpha", sql: `CREATE TABLE migration_alpha(value TEXT)`}
	b := migration{id: "20990101000002-add-beta", sql: `CREATE TABLE migration_beta(value TEXT)`}
	for _, first := range []migration{a, b} {
		for _, incoming := range [][]migration{{a, b}, {b, a}} {
			t.Run(first.id+"/"+incoming[0].id, func(t *testing.T) {
				s := newMigrationIDTestStore(t)
				if err := s.migrateWithMigrations(t.Context(), nil, legacyMigrations); err != nil {
					t.Fatal(err)
				}
				var columns int
				if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('schema_migrations') WHERE name='id'`).Scan(&columns); err != nil || columns != 0 {
					t.Fatalf("integer ledger was changed: columns=%d err=%v", columns, err)
				}
				if _, err := s.db.Exec(`UPDATE schema_migrations SET applied_at=123`); err != nil {
					t.Fatal(err)
				}
				branch, err := orderMigrations(append(append([]migration(nil), migrations...), first))
				if err != nil {
					t.Fatal(err)
				}
				if err = s.migrateWithMigrations(t.Context(), nil, branch); err != nil {
					t.Fatal(err)
				}
				var firstReceipt, firstTime int64
				if err = s.db.QueryRow(`SELECT version,applied_at FROM schema_migrations WHERE id=?`, first.id).Scan(&firstReceipt, &firstTime); err != nil {
					t.Fatal(err)
				}
				merged, err := orderMigrations(append(append([]migration(nil), migrations...), incoming...))
				if err != nil {
					t.Fatal(err)
				}
				if merged[len(merged)-2].id != a.id || merged[len(merged)-1].id != b.id {
					t.Fatal("pending migrations are not in lexical order")
				}
				for range 2 {
					if err = s.migrateWithMigrations(t.Context(), nil, merged); err != nil {
						t.Fatal(err)
					}
				}
				var preserved, receiptCount, tables int
				if err = s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version<=55 AND id=CAST(version AS TEXT) AND applied_at=123`).Scan(&preserved); err != nil || preserved != 55 {
					t.Fatalf("old receipts changed: %d %v", preserved, err)
				}
				if err = s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE id=? AND version=? AND applied_at=?`, first.id, firstReceipt, firstTime).Scan(&preserved); err != nil || preserved != 1 {
					t.Fatalf("branch receipt changed: %d %v", preserved, err)
				}
				if err = s.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&receiptCount); err != nil || receiptCount != len(merged) {
					t.Fatalf("receipt count=%d: %v", receiptCount, err)
				}
				if err = s.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name IN ('migration_alpha','migration_beta')`).Scan(&tables); err != nil || tables != 2 {
					t.Fatalf("merged tables=%d: %v", tables, err)
				}
				if _, _, err = readAppliedMigrationsWith(t.Context(), s.db, branch); !errors.Is(err, ErrSchemaTooNew) {
					t.Fatalf("downgrade to branch accepted: %v", err)
				}
			})
		}
	}
}

func TestMigrationIDsRollbackIncludesLedgerTransition(t *testing.T) {
	s := newMigrationIDTestStore(t)
	if err := s.migrateWithMigrations(t.Context(), nil, legacyMigrations); err != nil {
		t.Fatal(err)
	}
	broken, err := orderMigrations(append(append([]migration(nil), migrations...), migration{id: "20990101000003-broken", sql: `CREATE TABLE migration_partial(value TEXT); INSERT INTO nonexistent_table VALUES(1)`}))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.migrateWithMigrations(t.Context(), nil, broken); err == nil {
		t.Fatal("invalid SQL accepted")
	}
	var columns, tables, receipts int
	if err = s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('schema_migrations') WHERE name='id'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("ledger transition not rolled back: %d %v", columns, err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='migration_partial'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("migration DDL not rolled back: %d %v", tables, err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&receipts); err != nil || receipts != 55 {
		t.Fatalf("receipt not rolled back: %d %v", receipts, err)
	}
}

func TestMigrationIDsRejectInvalidDefinitions(t *testing.T) {
	for _, id := range []string{"54", "20260230030000-invalid-date", "2026110403000-short", "20261104030000-Uppercase", "20261104030000-../escape", "20261104030000-"} {
		t.Run(id, func(t *testing.T) {
			_, err := orderMigrations(append(append([]migration(nil), migrations...), migration{id: id, sql: `SELECT 1`}))
			if !errors.Is(err, ErrSchemaIncompatible) {
				t.Fatalf("invalid ID accepted: %v", err)
			}
		})
	}
	_, err := orderMigrations(append(append([]migration(nil), migrations...), migration{version: 56, sql: `SELECT 1`}))
	if !errors.Is(err, ErrSchemaIncompatible) {
		t.Fatalf("new integer migration accepted: %v", err)
	}
	_, err = orderMigrations(append(append([]migration(nil), migrations...), migrations[len(migrations)-1]))
	if !errors.Is(err, ErrSchemaIncompatible) || !strings.Contains(err.Error(), "duplicate migration ID") {
		t.Fatalf("duplicate ID accepted: %v", err)
	}
}

// This is also the CI guard: the integer catalog is frozen, including its bound.
func TestMigrationIDsNoNewIntegerMigrations(t *testing.T) {
	if lastIntegerMigrationVersion != 55 || len(legacyMigrations) != 55 {
		t.Fatal("integer migrations are frozen at 55; use hack/new-migration.py")
	}
	if migrationDefinitionError != nil {
		t.Fatal(migrationDefinitionError)
	}
	if err := validateMigrations(migrations, latestMigrationVersion); err != nil {
		t.Fatal(err)
	}
}

func newMigrationIDTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migration.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Store{db: db, now: time.Now, protocol: DefaultProtocolConfig()}
}

func TestMigrationIDsBackupRejectsUnknownIDEvenAtSameCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.sqlite")
	s, err := CreateGenesis(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE schema_migrations SET id='20991231235959-unknown' WHERE version=(SELECT max(version) FROM schema_migrations)`); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	before := readAuthoritativeDatabaseFiles(t, path)
	var output bytes.Buffer
	if _, err = WriteVerifiedBackup(t.Context(), path, latestMigrationVersion, Options{}, &output); !errors.Is(err, ErrSchemaTooNew) || output.Len() != 0 {
		t.Fatalf("unknown ID backup produced %d bytes: %v", output.Len(), err)
	}
	// Online backup readers can update SQLite's derivative SHM read marks.
	// The database and committed WAL must remain byte-for-byte unchanged.
	after := readAuthoritativeDatabaseFiles(t, path)
	delete(before, "-shm")
	delete(after, "-shm")
	assertDatabaseFilesEqual(t, after, before, "backup ID refusal")
}

func TestMigrationIDsLoadSeparateFilesAndApplyInLexicalOrder(t *testing.T) {
	files := fstest.MapFS{
		"migrations/20990101000002-beta.sql":  &fstest.MapFile{Data: []byte(`CREATE TABLE migration_beta(v TEXT)`)},
		"migrations/20990101000001-alpha.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE migration_alpha(v TEXT)`)},
	}
	items, err := loadMigrations(files)
	if err != nil {
		t.Fatal(err)
	}
	s := newMigrationIDTestStore(t)
	if err = s.migrateWithMigrations(t.Context(), nil, legacyMigrations); err != nil {
		t.Fatal(err)
	}
	if err = s.migrateWithMigrations(t.Context(), nil, items); err != nil {
		t.Fatal(err)
	}
	var ids string
	if err = s.db.QueryRow(`SELECT group_concat(id, ',') FROM (SELECT id FROM schema_migrations WHERE version>55 ORDER BY version)`).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	if ids != "20990101000001-alpha,20990101000002-beta" {
		t.Fatalf("applied order=%s", ids)
	}
	// The same loader used by embedded production SQL refuses newly numbered
	// files. This is independent of the frozen Go catalog's length guard.
	files["migrations/54.sql"] = &fstest.MapFile{Data: []byte(`SELECT 1`)}
	if _, err = loadMigrations(files); !errors.Is(err, ErrSchemaIncompatible) {
		t.Fatalf("integer SQL file accepted: %v", err)
	}
}

func TestMigrationIDsUpgradeFromV55PreservesIntegerReceipts(t *testing.T) {
	s := newMigrationIDTestStore(t)
	if err := s.migrateWithMigrations(t.Context(), nil, legacyMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE schema_migrations SET applied_at=version*100;
 CREATE TRIGGER frozen_integer_receipts BEFORE UPDATE OF version,applied_at ON schema_migrations
 WHEN OLD.version<=55 BEGIN SELECT RAISE(ABORT,'integer receipt changed'); END;`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.migrateWithMigrations(t.Context(), nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
	var preserved, timestamps int
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version<=55 AND id=CAST(version AS TEXT) AND applied_at=version*100`).Scan(&preserved); err != nil || preserved != 55 {
		t.Fatal("integer receipts changed", preserved, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version>55 AND length(id)>14`).Scan(&timestamps); err != nil || timestamps != len(migrations)-55 {
		t.Fatal("timestamp ordinals overlap integers", timestamps, err)
	}
	assertSchemaContains(t, s.db, "trigger", "promotions_branch_ci_immutable", "branch_ci_skipped")
	assertSchemaContains(t, s.db, "table", "issue_trailer_publications", "publication_id", "previous_sha")
	if _, _, err := readAppliedMigrationsWith(t.Context(), s.db, legacyMigrations); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatal("integer-only downgrade accepted", err)
	}
}
