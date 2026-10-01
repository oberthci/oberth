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
	database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, witnessGenesisAdoption{}, func(*store.Store) error {
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
	database, err := openStartupDatabase(context.Background(), path, &staticStartupContinuity{}, witnessChainReset{}, witnessGenesisAdoption{}, func(*store.Store) error {
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

// genesisRaceContinuity simulates a TOCTOU race by creating a valid genesis
// database when Pinned is called, which happens between openStartupDatabase's
// lstat (which found no file) and store.CreateGenesis's exclusive open.
type genesisRaceContinuity struct {
	path string
}

func (c *genesisRaceContinuity) Intents(context.Context) ([]model.AuditWitnessIntent, error) {
	return nil, nil
}

func (c *genesisRaceContinuity) Pinned(ctx context.Context) ([]model.AuditWitness, error) {
	database, err := store.CreateGenesis(ctx, c.path, store.Options{})
	if err != nil {
		return nil, err
	}
	return nil, database.Close()
}

func (c *genesisRaceContinuity) Prepare(context.Context, model.AuditWitnessIntent) error {
	return nil
}

// emptyFileRaceContinuity simulates a TOCTOU race where only an empty file
// is left behind (the creator crashed before initializing the schema).
type emptyFileRaceContinuity struct {
	path string
}

func (c *emptyFileRaceContinuity) Intents(context.Context) ([]model.AuditWitnessIntent, error) {
	return nil, nil
}

func (c *emptyFileRaceContinuity) Pinned(_ context.Context) ([]model.AuditWitness, error) {
	file, err := os.OpenFile(c.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return nil, file.Close()
}

func (c *emptyFileRaceContinuity) Prepare(context.Context, model.AuditWitnessIntent) error {
	return nil
}

func TestOpenStartupDatabaseAdoptsGenesisOnTOCTOURace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	continuity := &genesisRaceContinuity{path: path}
	verifyCalled := false
	database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, witnessGenesisAdoption{}, func(inspection *store.Store) error {
		verifyCalled = true
		_, err := inspection.VerifyAuditState(context.Background())
		return err
	})
	if err != nil {
		t.Fatalf("genesis with TOCTOU race: %v", err)
	}
	if !verifyCalled {
		t.Fatal("existing database verifier not called when adopting raced genesis")
	}
	head, err := database.VerifyAuditState(context.Background())
	if err != nil {
		_ = database.Close()
		t.Fatalf("verify adopted genesis audit state: %v", err)
	}
	if head.ID != 0 {
		_ = database.Close()
		t.Fatalf("adopted genesis audit head ID = %d, want 0 (empty)", head.ID)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.InspectCurrent(context.Background(), path, store.Options{})
	if err != nil {
		t.Fatalf("inspect adopted genesis: %v", err)
	}
	if err := inspection.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenStartupDatabaseRejectsEmptyFileOnTOCTOURace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	continuity := &emptyFileRaceContinuity{path: path}
	database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, witnessGenesisAdoption{}, func(*store.Store) error {
		t.Fatal("verifier ran for invalid adopted database")
		return nil
	})
	if database != nil {
		_ = database.Close()
	}
	if err == nil {
		t.Fatal("expected error when adopting empty file from TOCTOU race")
	}
	if !strings.Contains(err.Error(), "adopt genesis database that appeared during startup") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestOpenStartupDatabaseMigratesLegacyV1OnlyWithEmptyContinuity(t *testing.T) {
	t.Run("empty external continuity", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth-v1.sqlite")
		seedLegacyV1Database(t, path, 1)
		continuity := &staticStartupContinuity{}
		verifyCalled := false
		database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, witnessGenesisAdoption{}, func(inspection *store.Store) error {
			verifyCalled = true
			_, err := inspection.VerifyAuditState(context.Background())
			return err
		})
		if database != nil {
			_ = database.Close()
			t.Fatal("legacy startup returned a v2 database to a v3 daemon")
		}
		if !errors.Is(err, store.ErrSchemaIncompatible) || !strings.Contains(err.Error(), "backup-and-replace") {
			t.Fatalf("legacy startup error = %v, want backup-and-replace", err)
		}
		if verifyCalled {
			t.Fatal("v3 daemon verified or served the bounded v2 migration result")
		}
		if len(continuity.intents) != 1 || continuity.intents[0].Sequence != 1 {
			t.Fatalf("legacy migration intents = %#v, want one prepared intent", continuity.intents)
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
		database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, witnessGenesisAdoption{}, func(inspection *store.Store) error {
			_, err := inspection.VerifyAuditState(context.Background())
			return err
		})
		if database != nil {
			_ = database.Close()
			t.Fatal("legacy startup returned a v2 database to a v3 daemon")
		}
		if !errors.Is(err, store.ErrSchemaIncompatible) || !strings.Contains(err.Error(), "backup-and-replace") {
			t.Fatalf("resume prepared legacy migration error = %v, want backup-and-replace", err)
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
		database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, witnessGenesisAdoption{}, func(*store.Store) error {
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
		database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, witnessGenesisAdoption{}, func(*store.Store) error {
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
		database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, witnessGenesisAdoption{}, func(*store.Store) error {
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
		database, err = openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, witnessGenesisAdoption{}, func(inspection *store.Store) error {
			_, err := inspection.VerifyAuditState(context.Background())
			return err
		})
		if database != nil {
			_ = database.Close()
			t.Fatal("legacy startup returned a v2 database to a v3 daemon")
		}
		if !errors.Is(err, store.ErrSchemaIncompatible) || !strings.Contains(err.Error(), "backup-and-replace") {
			t.Fatalf("resume after ambiguous intent create error = %v, want backup-and-replace", err)
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
		database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, witnessGenesisAdoption{}, verify)
		if database != nil {
			_ = database.Close()
		}
		if !errors.Is(err, store.ErrSchemaIncompatible) || !strings.Contains(err.Error(), "backup-and-replace") {
			t.Fatalf("post-migration startup error = %v, want backup-and-replace", err)
		}
		if verifyCalls != 0 || len(continuity.intents) != 1 {
			t.Fatalf("v3 daemon verification state: calls=%d intents=%d", verifyCalls, len(continuity.intents))
		}
	})

	t.Run("empty legacy audit history needs no migration intent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth-v1.sqlite")
		seedLegacyV1Database(t, path, 0)
		continuity := &staticStartupContinuity{}
		database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, witnessGenesisAdoption{}, func(inspection *store.Store) error {
			_, err := inspection.VerifyAuditState(context.Background())
			return err
		})
		if database != nil {
			_ = database.Close()
			t.Fatal("legacy startup returned a v2 database to a v3 daemon")
		}
		if !errors.Is(err, store.ErrSchemaIncompatible) || !strings.Contains(err.Error(), "backup-and-replace") {
			t.Fatalf("empty legacy startup error = %v, want backup-and-replace", err)
		}
		if len(continuity.intents) != 0 {
			t.Fatalf("empty legacy migration created %d intents", len(continuity.intents))
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
	database, err := openStartupDatabase(context.Background(), path, continuity, witnessChainReset{}, witnessGenesisAdoption{}, func(*store.Store) error {
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

// TestOpenStartupDatabaseRefusesExactV2ForBackupAndReplace pins the migration
// philosophy: v1 -> v2 is the sole ratified live upgrade, so an existing v2
// database is refused byte-exactly with explicit backup-and-replace guidance
// instead of being rewritten in place by a newly deployed v3 binary.
func TestOpenStartupDatabaseRefusesExactV2ForBackupAndReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth-v2.sqlite")
	_, _ = seedPreviousV2Database(t, path)
	before := readAuthoritativeSQLiteFiles(t, path)
	database, err := openStartupDatabase(context.Background(), path, &staticStartupContinuity{}, witnessChainReset{}, witnessGenesisAdoption{}, func(*store.Store) error {
		t.Fatal("verifier ran for refused v2 schema")
		return nil
	})
	if database != nil {
		_ = database.Close()
	}
	if !errors.Is(err, store.ErrSchemaIncompatible) || !strings.Contains(err.Error(), "backup-and-replace") {
		t.Fatalf("existing v2 startup error = %v, want backup-and-replace refusal", err)
	}
	assertSQLiteFilesEqual(t, before, readAuthoritativeSQLiteFiles(t, path))
}

func TestOpenStartupDatabaseRejectsMalformedV2WithoutChangingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth-v2-malformed.sqlite")
	_, _ = seedPreviousV2Database(t, path)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE step_results ADD COLUMN unexpected TEXT`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	before := readAuthoritativeSQLiteFiles(t, path)
	database, err := openStartupDatabase(context.Background(), path, &staticStartupContinuity{}, witnessChainReset{}, witnessGenesisAdoption{}, func(*store.Store) error {
		t.Fatal("verifier ran for malformed v2 schema")
		return nil
	})
	if database != nil {
		_ = database.Close()
	}
	if !errors.Is(err, store.ErrSchemaIncompatible) {
		t.Fatalf("malformed v2 startup error = %v, want non-migratable schema rejection", err)
	}
	assertSQLiteFilesEqual(t, before, readAuthoritativeSQLiteFiles(t, path))
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
	opened, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, witnessChainReset{}, witnessGenesisAdoption{}, func(inspection *store.Store) error {
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
	opened, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, witnessChainReset{}, witnessGenesisAdoption{}, func(inspection *store.Store) error {
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

func seedPreviousV2Database(t *testing.T, path string) (model.AuditHead, string) {
	t.Helper()
	ctx := context.Background()
	database, err := store.CreateGenesis(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "forgejo", BaseURL: "https://codeberg.org",
	})
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	repository, err := database.CreateRepository(ctx, model.RepositorySpec{
		Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	run, err := database.EnqueueRun(ctx, model.RunSpec{
		RepoID: repository.ID, RefKind: model.RefBranch, Ref: "feature/v2",
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "SHA256:test", Trigger: "push",
	})
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if _, err := database.PutStepResult(ctx, model.StepResult{
		RunID: run.ID, Burn: "test", Step: "unit", Ordinal: 0,
		Status: model.StepPassed, ExitCode: 0, LogStart: 10, LogEnd: 20,
		DeclaredSize: "XL", MaxRSSBytes: 8192 << 10, UserCPU: 100 * time.Millisecond, SystemCPU: 25 * time.Millisecond,
	}); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test", ResourceType: "startup", ResourceID: "v2",
	}); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	head, err := database.VerifyAuditState(ctx)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
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
DROP TABLE IF EXISTS trusted_applies;
DROP TABLE IF EXISTS trusted_plans;
DROP TABLE IF EXISTS secret_access;
ALTER TABLE step_results DROP COLUMN system_cpu_ns;
ALTER TABLE step_results DROP COLUMN user_cpu_ns;
ALTER TABLE step_results DROP COLUMN max_rss_bytes;
ALTER TABLE step_results DROP COLUMN declared_size;
ALTER TABLE uplinks DROP COLUMN admin;
ALTER TABLE upstreams DROP COLUMN key_name;
ALTER TABLE runs DROP COLUMN credentialed;
ALTER TABLE runs DROP COLUMN concurrency_group;
DROP TRIGGER publications_guard_update;
CREATE TRIGGER publications_guard_update
BEFORE UPDATE ON publications
WHEN NOT (
    OLD.status = 'pending'
    AND NEW.sequence = OLD.sequence
    AND NEW.id = OLD.id
    AND NEW.repo_id = OLD.repo_id
    AND NEW.run_id IS OLD.run_id
    AND NEW.promotion_id IS OLD.promotion_id
    AND NEW.ref_kind = OLD.ref_kind
    AND NEW.ref = OLD.ref
    AND NEW.result_sha = OLD.result_sha
    AND NEW.actor = OLD.actor
    AND NEW.created_at = OLD.created_at
    AND (
        (
            NEW.status IN ('delivered', 'failed')
            AND NEW.previous_sha = OLD.previous_sha
            AND NEW.previous_known = OLD.previous_known
        )
        OR (
            NEW.status = 'pending'
            AND OLD.previous_known = 0
            AND OLD.previous_sha = ''
            AND NEW.previous_known = 1
            AND NEW.error = OLD.error
        )
    )
)
BEGIN
    SELECT RAISE(ABORT, 'publication transition is immutable');
END;
DROP TRIGGER ci_issue_projection_monotonic;
CREATE TRIGGER ci_issue_projection_monotonic
BEFORE UPDATE ON ci_issue_projections
WHEN NEW.repo_id != OLD.repo_id
  OR NEW.branch != OLD.branch
  OR NEW.origin != OLD.origin
  OR NEW.work_sequence <= OLD.work_sequence
BEGIN
    SELECT RAISE(ABORT, 'CI issue projection must advance monotonically');
END;
CREATE TABLE repositories_v1 (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    upstream_id INTEGER NOT NULL REFERENCES upstreams(id),
    default_branch TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
INSERT INTO repositories_v1 SELECT * FROM repositories;
DROP TABLE repositories;
ALTER TABLE repositories_v1 RENAME TO repositories;
DROP TABLE IF EXISTS schedule_fires;
DROP TABLE vm_suite_resources;
DROP TABLE vm_suite_executions;
DROP TABLE vm_capacity_slots;
DROP TABLE vm_executions;
DELETE FROM schema_migrations WHERE version > 2;
COMMIT;
PRAGMA foreign_keys = ON;
`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	assertLegacySchemaLedger(t, raw, 2)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	return head, run.ID
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
CREATE TABLE step_results_v1 (
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    burn TEXT NOT NULL,
    step TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('passed', 'failed', 'skipped', 'timed_out')),
    exit_code INTEGER NOT NULL,
    log_start INTEGER NOT NULL CHECK (log_start >= 0),
    log_end INTEGER NOT NULL CHECK (log_end >= log_start),
    started_at INTEGER,
    finished_at INTEGER,
    recorded_at INTEGER NOT NULL,
    PRIMARY KEY (run_id, burn, step)
);
INSERT INTO step_results_v1(
    run_id, burn, step, ordinal, status, exit_code, log_start, log_end,
    started_at, finished_at, recorded_at
)
SELECT run_id, burn, step, ordinal, status, exit_code, log_start, log_end,
       started_at, finished_at, recorded_at
FROM step_results;
DROP INDEX step_results_order_idx;
DROP TABLE step_results;
ALTER TABLE step_results_v1 RENAME TO step_results;
CREATE INDEX step_results_order_idx ON step_results(run_id, ordinal, burn, step);
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
DROP TABLE IF EXISTS trusted_applies;
DROP TABLE IF EXISTS trusted_plans;
DROP TABLE IF EXISTS secret_access;
ALTER TABLE upstreams DROP COLUMN key_name;
CREATE TABLE uplinks_v1 (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    fingerprint TEXT NOT NULL UNIQUE,
    identity TEXT NOT NULL,
    token_credential_id TEXT NOT NULL REFERENCES token_credentials(id),
    auth_actor TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
INSERT INTO uplinks_v1(id, fingerprint, identity, token_credential_id, auth_actor, created_at, updated_at)
SELECT id, fingerprint, identity, token_credential_id, auth_actor, created_at, updated_at FROM uplinks;
DROP INDEX uplinks_token_credential_idx;
DROP TABLE uplinks;
ALTER TABLE uplinks_v1 RENAME TO uplinks;
CREATE UNIQUE INDEX uplinks_token_credential_idx ON uplinks(token_credential_id);
ALTER TABLE runs DROP COLUMN credentialed;
ALTER TABLE runs DROP COLUMN concurrency_group;
DROP TRIGGER publications_guard_update;
CREATE TRIGGER publications_guard_update
BEFORE UPDATE ON publications
WHEN NOT (
    OLD.status = 'pending'
    AND NEW.sequence = OLD.sequence
    AND NEW.id = OLD.id
    AND NEW.repo_id = OLD.repo_id
    AND NEW.run_id IS OLD.run_id
    AND NEW.promotion_id IS OLD.promotion_id
    AND NEW.ref_kind = OLD.ref_kind
    AND NEW.ref = OLD.ref
    AND NEW.result_sha = OLD.result_sha
    AND NEW.actor = OLD.actor
    AND NEW.created_at = OLD.created_at
    AND (
        (
            NEW.status IN ('delivered', 'failed')
            AND NEW.previous_sha = OLD.previous_sha
            AND NEW.previous_known = OLD.previous_known
        )
        OR (
            NEW.status = 'pending'
            AND OLD.previous_known = 0
            AND OLD.previous_sha = ''
            AND NEW.previous_known = 1
            AND NEW.error = OLD.error
        )
    )
)
BEGIN
    SELECT RAISE(ABORT, 'publication transition is immutable');
END;
DROP TRIGGER ci_issue_projection_monotonic;
CREATE TRIGGER ci_issue_projection_monotonic
BEFORE UPDATE ON ci_issue_projections
WHEN NEW.repo_id != OLD.repo_id
  OR NEW.branch != OLD.branch
  OR NEW.origin != OLD.origin
  OR NEW.work_sequence <= OLD.work_sequence
BEGIN
    SELECT RAISE(ABORT, 'CI issue projection must advance monotonically');
END;
CREATE TABLE repositories_v1 (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    upstream_id INTEGER NOT NULL REFERENCES upstreams(id),
    default_branch TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
INSERT INTO repositories_v1 SELECT * FROM repositories;
DROP TABLE repositories;
ALTER TABLE repositories_v1 RENAME TO repositories;
DROP TABLE IF EXISTS schedule_fires;
DROP TABLE vm_suite_resources;
DROP TABLE vm_suite_executions;
DROP TABLE vm_capacity_slots;
DROP TABLE vm_executions;
DELETE FROM schema_migrations WHERE version > 1;
COMMIT;
PRAGMA foreign_keys = ON;
`); err != nil {
		t.Fatal(err)
	}
	assertLegacySchemaLedger(t, raw, 1)
}

func assertLegacySchemaLedger(t *testing.T, database *sql.DB, version int) {
	t.Helper()
	var count, minimum, maximum int
	if err := database.QueryRow(`
SELECT count(*), COALESCE(MIN(version), 0), COALESCE(MAX(version), 0)
FROM schema_migrations`).Scan(&count, &minimum, &maximum); err != nil {
		t.Fatal(err)
	}
	if count != version || minimum != 1 || maximum != version {
		t.Fatalf("legacy fixture ledger has %d rows spanning %d..%d, want exact prefix 1..%d",
			count, minimum, maximum, version)
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
	database, err := openStartupDatabase(context.Background(), path, &staticStartupContinuity{}, reset, witnessGenesisAdoption{}, func(*store.Store) error {
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
	database, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, reset, witnessGenesisAdoption{}, func(*store.Store) error {
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
	database, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, reset, witnessGenesisAdoption{}, func(*store.Store) error {
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
	database, err := openStartupDatabase(context.Background(), path, continuity, reset, witnessGenesisAdoption{}, func(*store.Store) error {
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
	database, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, reset, witnessGenesisAdoption{}, func(inspection *store.Store) error {
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
		first, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, reset, witnessGenesisAdoption{}, func(*store.Store) error {
			t.Fatal("verifier ran for fresh reset genesis")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		resumed, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, reset, witnessGenesisAdoption{}, func(*store.Store) error {
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
		first, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, reset, witnessGenesisAdoption{}, func(*store.Store) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		other := witnessChainReset{acknowledgedTip: strings.Repeat("e", 64), chain: tips}
		database, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, other, witnessGenesisAdoption{}, func(*store.Store) error { return nil })
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
		database, err := openStartupDatabase(ctx, path, &staticStartupContinuity{}, reset, witnessGenesisAdoption{}, func(*store.Store) error {
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
