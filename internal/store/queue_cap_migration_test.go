package store

import (
	"errors"
	"testing"
)

func TestQueueCapTimestampUpgradePreservesIntegerReceiptsAndConfiguration(t *testing.T) {
	const id = "20261104130000-queue-cap"
	s := newMigrationIDTestStore(t)
	ctx := t.Context()
	if err := s.migrateWithMigrations(ctx, nil, legacyMigrations); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE schema_migrations SET applied_at=version*100;
CREATE TRIGGER preserve_integer_receipts BEFORE UPDATE OF version,applied_at ON schema_migrations
WHEN OLD.version<=55 BEGIN SELECT RAISE(ABORT,'integer receipt changed'); END;`); err != nil {
		t.Fatal(err)
	}
	var tables int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name IN ('queue_cap','deferred_branch_receives')`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("integer catalog unexpectedly contains queue-cap tables: %d %v", tables, err)
	}
	if err := s.migrateWithMigrations(ctx, nil, migrations); err != nil {
		t.Fatal(err)
	}
	var limit, deferred, preserved, receipt int
	if err := s.db.QueryRow(`SELECT max_not_started FROM queue_cap WHERE singleton=1`).Scan(&limit); err != nil || limit != 0 {
		t.Fatalf("upgraded cap must default off: %d %v", limit, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM deferred_branch_receives`).Scan(&deferred); err != nil || deferred != 0 {
		t.Fatalf("upgrade invented deferred receives: %d %v", deferred, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version<=55 AND id=CAST(version AS TEXT) AND applied_at=version*100`).Scan(&preserved); err != nil || preserved != 55 {
		t.Fatalf("integer receipts changed: %d %v", preserved, err)
	}
	if err := s.db.QueryRow(`SELECT version FROM schema_migrations WHERE id=?`, id).Scan(&receipt); err != nil || receipt <= 55 {
		t.Fatalf("queue cap lacks a timestamp receipt: %d %v", receipt, err)
	}
	if _, err := s.db.Exec(`UPDATE queue_cap SET max_not_started=2 WHERE singleton=1`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.migrateWithMigrations(ctx, nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.QueryRow(`SELECT max_not_started FROM queue_cap WHERE singleton=1`).Scan(&limit); err != nil || limit != 2 {
		t.Fatalf("migration replay reset configured cap: %d %v", limit, err)
	}
	var receipts int
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE id=? AND version=?`, id, receipt).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("queue-cap receipt changed on replay: %d %v", receipts, err)
	}
	if _, _, err := readAppliedMigrationsWith(ctx, s.db, legacyMigrations); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("integer-only binary accepted timestamp upgrade: %v", err)
	}
}
