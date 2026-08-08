package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/auditanchor"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type staticStartupContinuity struct {
	intents               []model.AuditWitnessIntent
	pinned                []model.AuditWitness
	err                   error
	prepareErrAfterRecord error
}

func (continuity *staticStartupContinuity) Intents(context.Context) ([]model.AuditWitnessIntent, error) {
	return append([]model.AuditWitnessIntent(nil), continuity.intents...), continuity.err
}

func (continuity *staticStartupContinuity) Pinned(context.Context) ([]model.AuditWitness, error) {
	return append([]model.AuditWitness(nil), continuity.pinned...), continuity.err
}

func (continuity *staticStartupContinuity) Prepare(_ context.Context, intent model.AuditWitnessIntent) error {
	if continuity.err != nil {
		return continuity.err
	}
	intent.AuditSHA256 = append([]byte(nil), intent.AuditSHA256...)
	continuity.intents = append(continuity.intents, intent)
	return continuity.prepareErrAfterRecord
}

func TestOpenStartupDatabaseRequiresEmptyContinuityForGenesis(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	continuity := &staticStartupContinuity{intents: []model.AuditWitnessIntent{{Sequence: 1}}}
	database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, func(*store.Store) error {
		t.Fatal("existing database verifier ran for an absent database")
		return nil
	})
	if database != nil {
		_ = database.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "refuse database genesis") {
		t.Fatalf("genesis with external history error = %v", err)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("database created before empty-continuity proof: %v", statErr)
	}
}

func TestOpenStartupDatabaseCreatesVerifiedGenesis(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	verifyCalled := false
	database, err := openStartupDatabase(context.Background(), path, &staticStartupContinuity{}, witnessChainReset{}, func(*store.Store) error {
		verifyCalled = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if verifyCalled {
		t.Fatal("existing database verifier ran for fresh genesis")
	}
	if _, err := database.VerifyAuditState(context.Background()); err != nil {
		_ = database.Close()
		t.Fatalf("verify genesis audit state: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.InspectCurrent(context.Background(), path, store.Options{})
	if err != nil {
		t.Fatalf("inspect created genesis: %v", err)
	}
	if err := inspection.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenStartupDatabaseMigratesLegacyV1OnlyWithEmptyContinuity(t *testing.T) {
	t.Run("empty external continuity", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth-v1.sqlite")
		seedLegacyV1Database(t, path, 1)
		continuity := &staticStartupContinuity{}
		verifyCalled := false
		database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, func(inspection *store.Store) error {
			verifyCalled = true
			_, err := inspection.VerifyAuditState(context.Background())
			return err
		})
		if err != nil {
			t.Fatalf("migrate legacy startup database: %v", err)
		}
		defer func() { _ = database.Close() }()
		if !verifyCalled {
			t.Fatal("migrated schema was not re-inspected before writable startup")
		}
		head, err := database.VerifyAuditState(context.Background())
		if err != nil {
			t.Fatalf("verify migrated audit state: %v", err)
		}
		if head.ID != 1 {
			t.Fatalf("migrated audit head ID = %d, want 1", head.ID)
		}
		if len(continuity.intents) != 1 || continuity.intents[0].Sequence != 1 ||
			continuity.intents[0].AuditID != head.ID || !bytes.Equal(continuity.intents[0].AuditSHA256, head.SHA256) {
			t.Fatalf("legacy migration intent = %#v, want exact migrated head %#v", continuity.intents, head)
		}
	})

	t.Run("exact prepared intent resumes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth-v1.sqlite")
		seedLegacyV1Database(t, path, 1)
		head, err := store.InspectLegacyV1(context.Background(), path, store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		continuity := &staticStartupContinuity{intents: []model.AuditWitnessIntent{{
			Sequence: 1, AuditID: head.ID, AuditSHA256: append([]byte(nil), head.SHA256...),
		}}}
		database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, func(inspection *store.Store) error {
			_, err := inspection.VerifyAuditState(context.Background())
			return err
		})
		if err != nil {
			t.Fatalf("resume prepared legacy migration: %v", err)
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
		if len(continuity.intents) != 1 {
			t.Fatalf("resumed migration intents = %d, want no duplicate", len(continuity.intents))
		}
	})

	t.Run("existing external continuity", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth-v1.sqlite")
		seedLegacyV1Database(t, path, 1)
		before := readAuthoritativeSQLiteFiles(t, path)
		continuity := &staticStartupContinuity{intents: []model.AuditWitnessIntent{{Sequence: 1}}}
		database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, func(*store.Store) error {
			t.Fatal("current-schema verifier ran for a rejected legacy migration")
			return nil
		})
		if database != nil {
			_ = database.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "refuse legacy schema migration with rollback-external audit history") {
			t.Fatalf("legacy rollback error = %v", err)
		}
		after := readAuthoritativeSQLiteFiles(t, path)
		for suffix, expected := range before {
			if actual, ok := after[suffix]; !ok || !bytes.Equal(actual, expected) {
				t.Fatalf("authoritative SQLite file %q changed before legacy continuity approval", suffix)
			}
		}
	})

	t.Run("completed external continuity", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth-v1.sqlite")
		seedLegacyV1Database(t, path, 1)
		before := readAuthoritativeSQLiteFiles(t, path)
		continuity := &staticStartupContinuity{pinned: []model.AuditWitness{{UUID: "existing"}}}
		database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, func(*store.Store) error {
			t.Fatal("current-schema verifier ran for a rejected legacy migration")
			return nil
		})
		if database != nil {
			_ = database.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "refuse legacy schema migration with rollback-external audit history") {
			t.Fatalf("legacy completed-continuity error = %v", err)
		}
		after := readAuthoritativeSQLiteFiles(t, path)
		assertSQLiteFilesEqual(t, before, after)
	})

	t.Run("ambiguous intent create resumes without duplicate", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth-v1.sqlite")
		seedLegacyV1Database(t, path, 1)
		before := readAuthoritativeSQLiteFiles(t, path)
		wantErr := errors.New("intent create response lost")
		continuity := &staticStartupContinuity{prepareErrAfterRecord: wantErr}
		database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, func(*store.Store) error {
			t.Fatal("verifier ran before the intent create was acknowledged")
			return nil
		})
		if database != nil {
			_ = database.Close()
		}
		if !errors.Is(err, wantErr) || len(continuity.intents) != 1 {
			t.Fatalf("ambiguous intent result: error=%v intents=%#v", err, continuity.intents)
		}
		after := readAuthoritativeSQLiteFiles(t, path)
		assertSQLiteFilesEqual(t, before, after)

		continuity.prepareErrAfterRecord = nil
		database, err = openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, func(inspection *store.Store) error {
			_, err := inspection.VerifyAuditState(context.Background())
			return err
		})
		if err != nil {
			t.Fatalf("resume after ambiguous intent create: %v", err)
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
		if len(continuity.intents) != 1 {
			t.Fatalf("resume created %d intents, want exactly one", len(continuity.intents))
		}
	})

	t.Run("post-migration verification failure retries current schema", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth-v1.sqlite")
		seedLegacyV1Database(t, path, 3)
		continuity := &staticStartupContinuity{}
		wantErr := errors.New("external verification interrupted")
		verifyCalls := 0
		verify := func(inspection *store.Store) error {
			verifyCalls++
			if _, err := inspection.VerifyAuditState(context.Background()); err != nil {
				return err
			}
			if verifyCalls == 1 {
				return wantErr
			}
			return nil
		}
		database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, verify)
		if database != nil {
			_ = database.Close()
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("post-migration verification error = %v, want %v", err, wantErr)
		}
		inspection, inspectErr := store.InspectCurrent(context.Background(), path, store.Options{})
		if inspectErr != nil {
			t.Fatalf("migration did not commit before injected verification failure: %v", inspectErr)
		}
		if err := inspection.Close(); err != nil {
			t.Fatal(err)
		}

		database, err = openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, verify)
		if err != nil {
			t.Fatalf("retry post-migration verification: %v", err)
		}
		defer func() { _ = database.Close() }()
		head, err := database.VerifyAuditState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if verifyCalls != 2 || head.ID != 3 || len(continuity.intents) != 1 {
			t.Fatalf("retry state: verify calls=%d head=%d intents=%d", verifyCalls, head.ID, len(continuity.intents))
		}
	})

	t.Run("empty legacy audit history needs no migration intent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth-v1.sqlite")
		seedLegacyV1Database(t, path, 0)
		continuity := &staticStartupContinuity{}
		database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, func(inspection *store.Store) error {
			_, err := inspection.VerifyAuditState(context.Background())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = database.Close() }()
		head, err := database.VerifyAuditState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if head.ID != 0 || len(continuity.intents) != 0 {
			t.Fatalf("empty legacy migration: head=%d intents=%d", head.ID, len(continuity.intents))
		}
	})
}

func TestOpenStartupDatabaseDoesNotMigrateMalformedLegacyLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth-malformed-v1.sqlite")
	seedLegacyV1Database(t, path, 1)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE audit_actions ADD COLUMN sha256 BLOB`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	before := readAuthoritativeSQLiteFiles(t, path)
	continuity := &staticStartupContinuity{}
	database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, func(*store.Store) error {
		t.Fatal("verifier ran for malformed legacy schema")
		return nil
	})
	if database != nil {
		_ = database.Close()
	}
	if !errors.Is(err, store.ErrSchemaIncompatible) || errors.Is(err, store.ErrSchemaLegacyV1) {
		t.Fatalf("malformed legacy startup error = %v, want non-migratable schema rejection", err)
	}
	if len(continuity.intents) != 0 {
		t.Fatalf("malformed legacy startup created %d intents", len(continuity.intents))
	}
	after := readAuthoritativeSQLiteFiles(t, path)
	assertSQLiteFilesEqual(t, before, after)
}

func TestOpenStartupDatabaseLeavesRejectedExistingStateByteExact(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.CreateGenesis(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test", ResourceType: "startup", ResourceID: "rollback",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	before := readAuthoritativeSQLiteFiles(t, path)
	wantErr := errors.New("rollback-external continuity rejected local snapshot")
	opened, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, witnessChainReset{}, func(inspection *store.Store) error {
		if _, verifyErr := inspection.VerifyAuditState(ctx); verifyErr != nil {
			t.Fatalf("read-only local verification: %v", verifyErr)
		}
		return wantErr
	})
	if opened != nil {
		_ = opened.Close()
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("existing state rejection error = %v, want %v", err, wantErr)
	}
	after := readAuthoritativeSQLiteFiles(t, path)
	for suffix, expected := range before {
		if actual, ok := after[suffix]; !ok || !bytes.Equal(actual, expected) {
			t.Fatalf("authoritative SQLite file %q changed before continuity approval", suffix)
		}
	}
	for suffix := range after {
		if _, ok := before[suffix]; !ok {
			t.Fatalf("authoritative SQLite file %q appeared before continuity approval", suffix)
		}
	}
}

func TestOpenStartupDatabasePropagatesWitnessUnavailableWithIntactDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.CreateGenesis(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test", ResourceType: "startup", ResourceID: "rekor-down",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	rekorErr := fmt.Errorf("audit anchor: startup recover external witness history: %w: search Rekor: connection refused", auditanchor.ErrWitnessUnavailable)
	opened, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, witnessChainReset{}, func(inspection *store.Store) error {
		// Local verification passes; only external witness recovery fails.
		if _, verifyErr := inspection.VerifyAuditState(ctx); verifyErr != nil {
			t.Fatalf("local verification should pass: %v", verifyErr)
		}
		return rekorErr
	})
	if opened != nil {
		_ = opened.Close()
	}
	if !errors.Is(err, auditanchor.ErrWitnessUnavailable) {
		t.Fatalf("error = %v, want wrapped ErrWitnessUnavailable", err)
	}
	// The database should be safe to reopen: local integrity was verified
	// before the external witness recovery failed.
	reopened, err := store.OpenCurrent(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("reopen after deferred witness recovery: %v", err)
	}
	head, err := reopened.VerifyAuditState(ctx)
	if err != nil {
		_ = reopened.Close()
		t.Fatalf("verify audit state after reopen: %v", err)
	}
	if head.ID != 1 {
		_ = reopened.Close()
		t.Fatalf("reopened audit head ID = %d, want 1", head.ID)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func readAuthoritativeSQLiteFiles(t *testing.T, path string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte, 3)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		body, err := os.ReadFile(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		files[suffix] = body
	}
	return files
}

func assertSQLiteFilesEqual(t *testing.T, expected, actual map[string][]byte) {
	t.Helper()
	if len(actual) != len(expected) {
		t.Fatalf("authoritative SQLite file count = %d, want %d", len(actual), len(expected))
	}
	for suffix, body := range expected {
		if !bytes.Equal(actual[suffix], body) {
			t.Fatalf("authoritative SQLite file %q changed", suffix)
		}
	}
}

func seedLegacyV1Database(t *testing.T, path string, actionCount int) {
	t.Helper()
	ctx := context.Background()
	database, err := store.CreateGenesis(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= actionCount; index++ {
		if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
			Actor: "legacy@host", Action: "issue.create", ResourceType: "issue",
			ResourceID: fmt.Sprintf("%d", index), Details: fmt.Sprintf(`{"title":"legacy-%d"}`, index),
		}); err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`
PRAGMA foreign_keys = OFF;
BEGIN IMMEDIATE;
CREATE TABLE audit_actions_v1 (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    actor TEXT NOT NULL,
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    details TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
INSERT INTO audit_actions_v1(id, actor, action, resource_type, resource_id, details, created_at)
SELECT id, actor, action, resource_type, resource_id, details, created_at FROM audit_actions ORDER BY id;
DROP TRIGGER audit_actions_no_update;
DROP TRIGGER audit_actions_no_delete;
DROP INDEX audit_actions_page_idx;
DROP TABLE audit_actions;
ALTER TABLE audit_actions_v1 RENAME TO audit_actions;
CREATE INDEX audit_actions_page_idx ON audit_actions(id DESC);
CREATE TRIGGER audit_actions_no_update BEFORE UPDATE ON audit_actions
BEGIN SELECT RAISE(ABORT, 'audit actions are append-only'); END;
CREATE TRIGGER audit_actions_no_delete BEFORE DELETE ON audit_actions
BEGIN SELECT RAISE(ABORT, 'audit actions are append-only'); END;
DROP TABLE audit_anchors;
DROP TABLE IF EXISTS audit_migration_bootstrap;
DELETE FROM schema_migrations WHERE version = 2;
COMMIT;
PRAGMA foreign_keys = ON;
`); err != nil {
		t.Fatal(err)
	}
}

type staticChainTips struct {
	tip   model.AuditWitness
	found bool
	err   error
	calls int
}

func (tips *staticChainTips) AbandonedChainTip(context.Context) (model.AuditWitness, bool, error) {
	tips.calls++
	return tips.tip, tips.found, tips.err
}

func testAbandonedTip() model.AuditWitness {
	return model.AuditWitness{
		UUID: strings.Repeat("d", 64), LogIndex: 41,
		IntegratedAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		AuditID:      17, AuditSHA256: bytes.Repeat([]byte{0x5a}, 32),
	}
}

func TestOpenStartupDatabaseWitnessChainResetRequiresExactTip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	tips := &staticChainTips{tip: testAbandonedTip(), found: true}
	reset := witnessChainReset{acknowledgedTip: strings.Repeat("e", 64), chain: tips}
	database, err := openStartupDatabase(context.Background(), path, &staticStartupContinuity{}, reset, func(*store.Store) error {
		t.Fatal("existing database verifier ran for an absent database")
		return nil
	})
	if database != nil {
		_ = database.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "does not acknowledge the latest published Rekor witness") {
		t.Fatalf("wrong acknowledged tip error = %v", err)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("database created despite refused witness chain reset: %v", statErr)
	}
}

func TestOpenStartupDatabaseWitnessChainResetRecordsAcknowledgmentAsFirstAction(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	abandoned := testAbandonedTip()
	tips := &staticChainTips{tip: abandoned, found: true}
	reset := witnessChainReset{acknowledgedTip: abandoned.UUID, chain: tips}
	database, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, reset, func(*store.Store) error {
		t.Fatal("existing database verifier ran for fresh reset genesis")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	action, err := database.SoleAuditAction(ctx)
	if closeErr := database.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatalf("sole acknowledgment action: %v", err)
	}
	if action.ID != 1 || action.Actor != witnessChainResetActor || action.Action != witnessChainResetAction ||
		action.ResourceType != witnessChainResetResourceType || action.ResourceID != abandoned.UUID {
		t.Fatalf("acknowledgment action = %+v", action)
	}
	var details witnessChainResetDetails
	if err := json.Unmarshal([]byte(action.Details), &details); err != nil {
		t.Fatalf("acknowledgment details: %v", err)
	}
	if details.AbandonedTipUUID != abandoned.UUID || details.AbandonedTipLogIndex != abandoned.LogIndex ||
		details.AbandonedAuditID != abandoned.AuditID || details.AbandonedAuditSHA256 != "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a" {
		t.Fatalf("acknowledgment details = %+v", details)
	}
}

func TestOpenStartupDatabaseWitnessChainResetIsNoOpWithoutPublicHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	tips := &staticChainTips{}
	reset := witnessChainReset{acknowledgedTip: strings.Repeat("d", 64), chain: tips}
	database, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, reset, func(*store.Store) error {
		t.Fatal("existing database verifier ran for fresh genesis")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	head, err := database.VerifyAuditState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head.ID != 0 || tips.calls != 1 {
		t.Fatalf("no-history reset produced head %d with %d discovery calls, want plain genesis", head.ID, tips.calls)
	}
}

func TestOpenStartupDatabaseWitnessChainResetNeverOverridesExternalContinuity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	tips := &staticChainTips{tip: testAbandonedTip(), found: true}
	continuity := &staticStartupContinuity{intents: []model.AuditWitnessIntent{{Sequence: 1}}}
	reset := witnessChainReset{acknowledgedTip: testAbandonedTip().UUID, chain: tips}
	database, err := openStartupDatabase(context.Background(), path, continuity, reset, func(*store.Store) error {
		t.Fatal("existing database verifier ran for an absent database")
		return nil
	})
	if database != nil {
		_ = database.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "refuse database genesis") {
		t.Fatalf("reset with external continuity error = %v", err)
	}
	if tips.calls != 0 {
		t.Fatalf("reset consulted Rekor %d times despite rollback-external continuity evidence", tips.calls)
	}
}

func TestOpenStartupDatabaseWitnessChainResetIgnoredForEstablishedDatabases(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	seeded, err := store.CreateGenesis(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if _, err := seeded.AppendAuditAction(ctx, model.AuditActionSpec{
			Actor: "agent@host", Action: "test", ResourceType: "startup", ResourceID: id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := seeded.Close(); err != nil {
		t.Fatal(err)
	}
	tips := &staticChainTips{tip: testAbandonedTip(), found: true}
	reset := witnessChainReset{acknowledgedTip: testAbandonedTip().UUID, chain: tips}
	verified := false
	database, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, reset, func(inspection *store.Store) error {
		verified = true
		_, err := inspection.VerifyAuditState(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if !verified || tips.calls != 0 {
		t.Fatalf("established database: verified=%v discovery calls=%d, want unchanged fail-closed path", verified, tips.calls)
	}
}

func TestOpenStartupDatabaseWitnessChainResetResumesVirginStates(t *testing.T) {
	ctx := context.Background()
	abandoned := testAbandonedTip()

	t.Run("sole acknowledgment resumes without duplicate", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth.sqlite")
		tips := &staticChainTips{tip: abandoned, found: true}
		reset := witnessChainReset{acknowledgedTip: abandoned.UUID, chain: tips}
		first, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, reset, func(*store.Store) error {
			t.Fatal("verifier ran for fresh reset genesis")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		resumed, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, reset, func(*store.Store) error {
			t.Fatal("verifier ran for a virgin acknowledgment resume")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		action, actionErr := resumed.SoleAuditAction(ctx)
		if closeErr := resumed.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if actionErr != nil || action.ResourceID != abandoned.UUID {
			t.Fatalf("resumed acknowledgment = %+v, %v", action, actionErr)
		}
	})

	t.Run("sole acknowledgment refuses another tip", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth.sqlite")
		tips := &staticChainTips{tip: abandoned, found: true}
		reset := witnessChainReset{acknowledgedTip: abandoned.UUID, chain: tips}
		first, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, reset, func(*store.Store) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		other := witnessChainReset{acknowledgedTip: strings.Repeat("e", 64), chain: tips}
		database, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, other, func(*store.Store) error { return nil })
		if database != nil {
			_ = database.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "already acknowledges tip") {
			t.Fatalf("mismatched resume error = %v", err)
		}
	})

	t.Run("empty genesis gains the acknowledgment", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth.sqlite")
		empty, err := store.CreateGenesis(ctx, path, store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := empty.Close(); err != nil {
			t.Fatal(err)
		}
		tips := &staticChainTips{tip: abandoned, found: true}
		reset := witnessChainReset{acknowledgedTip: abandoned.UUID, chain: tips}
		database, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, reset, func(*store.Store) error {
			t.Fatal("verifier ran for an empty-genesis reset resume")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		action, actionErr := database.SoleAuditAction(ctx)
		if closeErr := database.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if actionErr != nil || action.ID != 1 || action.ResourceID != abandoned.UUID {
			t.Fatalf("empty-genesis resume acknowledgment = %+v, %v", action, actionErr)
		}
	})
}
