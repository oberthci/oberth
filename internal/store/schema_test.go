package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type sqliteResultError int

func (e sqliteResultError) Error() string { return "sqlite result" }

func (e sqliteResultError) Code() int { return int(e) }

func TestSchemaMigrationsAreContiguous(t *testing.T) {
	t.Parallel()

	if latestMigrationVersion != 2 {
		t.Fatalf("latest migration version = %d, want 2", latestMigrationVersion)
	}
	if len(migrations) != latestMigrationVersion {
		t.Fatalf("migration count = %d, want %d", len(migrations), latestMigrationVersion)
	}
	for index, item := range migrations {
		if item.version != index+1 {
			t.Fatalf("migration %d version = %d, want %d", index, item.version, index+1)
		}
		if (item.sql == "") == (item.apply == nil) {
			t.Fatalf("migration %d must define exactly one application method", item.version)
		}
	}

	// Version 1 remains the exact fresh-rewrite schema already deployed to the
	// cluster. Evolution belongs in later migrations, never in this baseline.
	schema := strings.ToLower(migrations[0].sql)
	for _, forbidden := range []string{
		"\nalter table ",
		"\ndrop table ",
		"\ndrop trigger ",
		"\nupdate ",
		"\ndelete from ",
		"step_results_v3",
		"step_results_v4",
		"run_cancellations_v5",
		"migration:v",
		"promotions_run_unique_",
		"backfill",
		"legacy",
		"compatib",
	} {
		if strings.Contains(schema, forbidden) {
			t.Errorf("fresh schema contains migration baggage %q", forbidden)
		}
	}
}

func TestSchemaUpgradesVersionOneAuditHistory(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "oberth-v1.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`
CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(migrations[0].sql); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`
INSERT INTO audit_actions(id, actor, action, resource_type, resource_id, details, created_at)
VALUES
    (1, 'first@host', 'issue.create', 'issue', '1', '{"title":"one"}', 1),
    (2, 'second@host', 'issue.update', 'issue', '1', '{"title":"two"}', 2)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	expectedHead, err := InspectLegacyV1(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("inspect v1 schema: %v", err)
	}
	upgraded, err := MigrateLegacyV1(context.Background(), path, expectedHead, Options{})
	if err != nil {
		t.Fatalf("upgrade v1 schema: %v", err)
	}
	defer func() { _ = upgraded.Close() }()
	assertFreshSchemaLedger(t, upgraded.db)

	head, err := upgraded.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatalf("verify upgraded audit chain: %v", err)
	}
	if head.ID != 2 {
		t.Fatalf("upgraded audit head ID = %d, want 2", head.ID)
	}
	action, err := upgraded.AppendAuditAction(context.Background(), model.AuditActionSpec{
		Actor: "third@host", Action: "issue.close", ResourceType: "issue", ResourceID: "1",
	})
	if err != nil {
		t.Fatalf("append after v1 upgrade: %v", err)
	}
	if action.ID != 3 || !slices.Equal(action.PreviousSHA256, head.SHA256) {
		t.Fatalf("post-upgrade audit action = %#v, want ID 3 chained to prior head", action)
	}
}

func TestConcurrentOpenersSerializeVersionOneUpgrade(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "oberth-v1-concurrent.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`PRAGMA journal_mode = WAL;` + `
CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);` + migrations[0].sql + `
INSERT INTO schema_migrations(version, applied_at) VALUES(1, 1);
INSERT INTO audit_actions(id, actor, action, resource_type, resource_id, details, created_at)
VALUES(1, 'agent@host', 'issue.create', 'issue', '1', '{"title":"one"}', 1);`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	expectedHead, err := InspectLegacyV1(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}

	const openerCount = 8
	start := make(chan struct{})
	errorsByOpener := make(chan error, openerCount)
	var openers sync.WaitGroup
	for range openerCount {
		openers.Add(1)
		go func() {
			defer openers.Done()
			<-start
			opened, openErr := MigrateLegacyV1(context.Background(), path, expectedHead, Options{BusyTimeout: 10 * time.Second})
			if opened != nil {
				openErr = errors.Join(openErr, opened.Close())
			}
			errorsByOpener <- openErr
		}()
	}
	close(start)
	openers.Wait()
	close(errorsByOpener)
	successes, exactRejections := 0, 0
	for openErr := range errorsByOpener {
		switch {
		case openErr == nil:
			successes++
		case errors.Is(openErr, ErrSchemaIncompatible) && strings.Contains(openErr.Error(), "expected schema version 1, found 2"):
			exactRejections++
		default:
			t.Fatalf("concurrent explicit migration: %v", openErr)
		}
	}
	if successes != 1 || exactRejections != openerCount-1 {
		t.Fatalf("concurrent explicit migrations: successes=%d exact rejections=%d, want 1/%d",
			successes, exactRejections, openerCount-1)
	}

	upgraded, err := OpenAdminClient(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upgraded.Close() }()
	assertFreshSchemaLedger(t, upgraded.db)
	head, err := upgraded.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if head.ID != 1 {
		t.Fatalf("audit head after concurrent upgrade = %d, want 1", head.ID)
	}
}

func TestLegacyMigrationRefusesChangedHeadBeforeWriting(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth-v1-stale-head.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`PRAGMA journal_mode = WAL;` + `
CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
` + migrations[0].sql + `
INSERT INTO schema_migrations(version, applied_at) VALUES(1, 1);
INSERT INTO audit_actions(id, actor, action, resource_type, resource_id, details, created_at)
VALUES(1, 'first@host', 'issue.create', 'issue', '1', '{"title":"one"}', 1);`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	staleHead, err := InspectLegacyV1(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	database, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
INSERT INTO audit_actions(id, actor, action, resource_type, resource_id, details, created_at)
VALUES(2, 'second@host', 'issue.update', 'issue', '1', '{"title":"two"}', 2)`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	before := readAuthoritativeDatabaseFiles(t, path)

	migrated, err := MigrateLegacyV1(ctx, path, staleHead, Options{})
	if migrated != nil {
		_ = migrated.Close()
	}
	if !errors.Is(err, ErrSchemaIncompatible) || !strings.Contains(err.Error(), "audit head changed") {
		t.Fatalf("stale-head migration error = %v", err)
	}
	after := readAuthoritativeDatabaseFiles(t, path)
	if len(after) != len(before) {
		t.Fatalf("stale-head refusal changed SQLite file count from %d to %d", len(before), len(after))
	}
	for suffix, expected := range before {
		if !bytes.Equal(after[suffix], expected) {
			t.Fatalf("stale-head refusal changed SQLite file %q", suffix)
		}
	}

	currentHead, err := InspectLegacyV1(ctx, path, Options{})
	if err != nil {
		t.Fatalf("inspect v1 after stale-head refusal: %v", err)
	}
	if currentHead.ID != 2 {
		t.Fatalf("v1 head after stale-head refusal = %d, want 2", currentHead.ID)
	}
	migrated, err = MigrateLegacyV1(ctx, path, currentHead, Options{})
	if err != nil {
		t.Fatalf("migrate exact current head: %v", err)
	}
	defer func() { _ = migrated.Close() }()
	verified, err := migrated.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if verified.ID != currentHead.ID || !bytes.Equal(verified.SHA256, currentHead.SHA256) {
		t.Fatalf("migrated head = %#v, want %#v", verified, currentHead)
	}
}

func TestInspectLegacyV1RejectsDowngradedV2Shape(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth-downgraded-v2.sqlite")
	database, err := CreateGenesis(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
DROP TABLE audit_anchors;
DELETE FROM schema_migrations WHERE version = 2;`); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := InspectLegacyV1(ctx, path, Options{}); !errors.Is(err, ErrSchemaIncompatible) ||
		errors.Is(err, ErrSchemaLegacyV1) || !strings.Contains(err.Error(), "not the exact predecessor") {
		t.Fatalf("downgraded v2 inspection error = %v, want non-legacy schema rejection", err)
	}
}

func TestInspectLegacyV1RequiresEveryCanonicalSchemaObject(t *testing.T) {
	t.Parallel()

	mutations := map[string]string{
		"alter unrelated table": `ALTER TABLE publications ADD COLUMN escaped TEXT`,
		"replace audit index": `
DROP INDEX audit_actions_page_idx;
CREATE INDEX audit_actions_page_idx ON audit_actions(created_at DESC);`,
		"replace audit trigger": `
DROP TRIGGER audit_actions_no_update;
CREATE TRIGGER audit_actions_no_update BEFORE UPDATE ON audit_actions BEGIN SELECT 1; END;`,
		"remove unrelated trigger": `DROP TRIGGER promotions_no_delete`,
		"add extra object":         `CREATE TABLE escaped_state(id INTEGER PRIMARY KEY)`,
	}
	for name, mutation := range mutations {
		name, mutation := name, mutation
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "oberth-v1.sqlite")
			createExactLegacyV1Database(t, path)
			database, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(mutation); err != nil {
				_ = database.Close()
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			before := readAuthoritativeDatabaseFiles(t, path)

			if _, err := InspectLegacyV1(context.Background(), path, Options{}); !errors.Is(err, ErrSchemaIncompatible) ||
				errors.Is(err, ErrSchemaLegacyV1) || !strings.Contains(err.Error(), "not the exact predecessor") {
				t.Fatalf("altered v1 inspection error = %v, want exact non-legacy rejection", err)
			}
			after := readAuthoritativeDatabaseFiles(t, path)
			assertDatabaseFilesEqual(t, after, before, "read-only exact-v1 rejection")
		})
	}
}

func TestLegacyMigrationRechecksEverySchemaObjectBeforeWriting(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth-v1.sqlite")
	createExactLegacyV1Database(t, path)
	head, err := InspectLegacyV1(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
DROP INDEX publications_pending_idx;
CREATE INDEX publications_pending_idx ON publications(sequence, status);`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	before := readAuthoritativeDatabaseFiles(t, path)

	migrated, err := MigrateLegacyV1(ctx, path, head, Options{})
	if migrated != nil {
		_ = migrated.Close()
	}
	if !errors.Is(err, ErrSchemaIncompatible) || errors.Is(err, ErrSchemaLegacyV1) ||
		!strings.Contains(err.Error(), "not the exact predecessor") {
		t.Fatalf("altered v1 migration error = %v, want exact non-legacy rejection", err)
	}
	after := readAuthoritativeDatabaseFiles(t, path)
	assertDatabaseFilesEqual(t, after, before, "transactional exact-v1 rejection")
}

func TestNormalizeSchemaSQLPreservesTokenAndQuotedValueBoundaries(t *testing.T) {
	t.Parallel()

	canonical, err := normalizeSchemaSQL("CREATE TABLE item (value TEXT DEFAULT 'two words')")
	if err != nil {
		t.Fatal(err)
	}
	formatted, err := normalizeSchemaSQL(" create\n table ITEM(value text default 'two words') ")
	if err != nil {
		t.Fatal(err)
	}
	if canonical != formatted {
		t.Fatal("formatting-only schema difference did not normalize")
	}
	quotedIdentifier, err := normalizeSchemaSQL(`CREATE TABLE "item" (value TEXT DEFAULT 'two words')`)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != quotedIdentifier {
		t.Fatal("equivalent quoted schema identifier did not normalize")
	}
	changedLiteral, err := normalizeSchemaSQL("CREATE TABLE item (value TEXT DEFAULT 'two  words')")
	if err != nil {
		t.Fatal(err)
	}
	if canonical == changedLiteral {
		t.Fatal("quoted schema value difference was erased")
	}
	separateTokens, err := normalizeSchemaSQL("CREATE TABLE item (value INTEGER PRIMARY KEY)")
	if err != nil {
		t.Fatal(err)
	}
	collidingIdentifier, err := normalizeSchemaSQL("CREATE TABLE item (value INTEGERPRIMARYKEY)")
	if err != nil {
		t.Fatal(err)
	}
	if separateTokens == collidingIdentifier {
		t.Fatal("schema token boundaries collided")
	}
}

func TestImmediateTransactionDSNConfiguresInitialBusyTimeout(t *testing.T) {
	t.Parallel()

	got, err := immediateTransactionDSN(
		"file.sqlite?mode=rwc&_pragma=foreign_keys(1)&_pragma=BUSY_TIMEOUT%3D1234&_txlock=deferred",
		1500*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "file.sqlite?_pragma=foreign_keys%281%29&_pragma=busy_timeout%281500%29&_txlock=immediate&mode=rwc"
	if got != want {
		t.Fatalf("DSN = %q, want %q", got, want)
	}
}

func TestOptionsBusyTimeoutOverridesDSNPragma(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "busy-override.sqlite") +
		"?_pragma=busy_timeout(1234)&_pragma=foreign_keys(1)"
	opened, err := OpenAdminClient(context.Background(), path, Options{BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	var timeout, foreignKeys int
	if err := opened.db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if err := opened.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if timeout != 50 {
		t.Fatalf("busy timeout = %dms, want Options-owned 50ms", timeout)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign keys = %d, want preserved pragma value 1", foreignKeys)
	}
}

func TestRetrySQLiteLockUsesTypedCodesAndStopsAfterSuccess(t *testing.T) {
	t.Parallel()

	attempts := 0
	err := retrySQLiteLock(context.Background(), time.Second, func(context.Context) error {
		attempts++
		switch attempts {
		case 1:
			return sqliteResultError(sqliteBusyCode)
		case 2:
			return sqliteResultError(sqliteLockedCode | 1<<8)
		default:
			return nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestRetrySQLiteLockDoesNotRetryUnrelatedErrors(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		want error
	}{
		{name: "ordinary", want: errors.New("permanent")},
		{name: "typed sqlite IOERR", want: sqliteResultError(10)},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			err := retrySQLiteLock(context.Background(), time.Second, func(context.Context) error {
				attempts++
				return test.want
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if attempts != 1 {
				t.Fatalf("attempts = %d, want 1", attempts)
			}
		})
	}
}

func TestRetrySQLiteLockHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempts := 0
	err := retrySQLiteLock(ctx, time.Second, func(context.Context) error {
		attempts++
		return sqliteResultError(sqliteBusyCode)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if attempts != 0 {
		t.Fatalf("attempts = %d, want 0 for an already canceled context", attempts)
	}
}

func TestRetrySQLiteLockPreservesPriorLockOnInFlightCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	err := retrySQLiteLock(ctx, time.Second, func(context.Context) error {
		attempts++
		if attempts == 1 {
			return sqliteResultError(sqliteBusyCode)
		}
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	var lock sqliteResultError
	if !errors.As(err, &lock) || lock.Code() != sqliteBusyCode {
		t.Fatalf("error = %v, want preserved SQLITE_BUSY cause", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestEnableWALBoundsDriverBusyWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal-busy.sqlite")
	blocker, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Close() }()
	if _, err := blocker.Exec(`CREATE TABLE lock_target (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	candidate, err := sql.Open("sqlite", path+`?_pragma=busy_timeout(1000)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = candidate.Close() }()
	candidate.SetMaxOpenConns(1)
	if err := candidate.Ping(); err != nil {
		t.Fatal(err)
	}

	lockConn, err := blocker.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockConn.Close() }()
	if _, err := lockConn.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = lockConn.ExecContext(context.Background(), `ROLLBACK`) }()

	started := time.Now()
	err = (&Store{db: candidate}).enableWAL(context.Background(), 50*time.Millisecond)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want bounded deadline", err)
	}
	var lock interface{ Code() int }
	if !errors.As(err, &lock) || lock.Code()&sqlitePrimaryMask != sqliteBusyCode {
		t.Fatalf("error = %v, want preserved SQLITE_BUSY cause", err)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("WAL retry elapsed %s, want less than half the driver's 1s busy timeout", elapsed)
	}
}

func TestFreshSchemaIsCompleteAndIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	store, err := OpenAdminClient(context.Background(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}

	assertFreshSchemaLedger(t, store.db)
	assertSchemaObjects(t, store.db, "table", []string{
		"audit_actions",
		"audit_anchors",
		"ci_issue_projections",
		"ci_issue_work",
		"issue_locks",
		"issues",
		"pending_token_credentials",
		"promotions",
		"publications",
		"receive_events",
		"repositories",
		"run_cancellations",
		"runs",
		"schema_identity",
		"schema_migrations",
		"step_results",
		"token_credentials",
		"uplinks",
		"upstreams",
	})
	assertSchemaObjects(t, store.db, "index", []string{
		"audit_actions_page_idx",
		"audit_anchors_latest_idx",
		"ci_issue_projection_failure_idx",
		"ci_issue_work_branch_idx",
		"ci_issue_work_promotion_idx",
		"ci_issue_work_run_idx",
		"issue_locks_expiry_idx",
		"issues_page_idx",
		"manual_issues_page_idx",
		"one_open_ci_issue_idx",
		"promotions_repo_idx",
		"promotions_run_idx",
		"publications_pending_idx",
		"publications_promotion_idx",
		"publications_run_idx",
		"receive_events_repo_idx",
		"receive_events_run_idx",
		"run_cancellations_pending_idx",
		"runs_fifo_idx",
		"runs_ref_idx",
		"runs_sha_idx",
		"step_results_order_idx",
		"uplinks_token_credential_idx",
	})
	assertSchemaObjects(t, store.db, "trigger", []string{
		"audit_actions_no_delete",
		"audit_actions_no_update",
		"audit_anchors_no_delete",
		"audit_anchors_no_update",
		"ci_issue_projection_monotonic",
		"ci_issue_work_no_delete",
		"ci_issue_work_no_update",
		"promotions_guard_update",
		"promotions_no_delete",
		"publications_guard_update",
		"publications_no_delete",
		"schema_identity_no_delete",
		"schema_identity_no_insert",
		"schema_identity_no_update",
	})

	expectedColumns := map[string][]string{
		"schema_identity":           {"singleton", "identity"},
		"schema_migrations":         {"version", "applied_at"},
		"upstreams":                 {"id", "name", "kind", "base_url", "created_at", "updated_at"},
		"repositories":              {"id", "name", "upstream_id", "default_branch", "created_at", "updated_at"},
		"runs":                      {"queue_sequence", "id", "repo_id", "ref_kind", "ref", "sha", "actor", "release", "trigger", "phase", "job_name", "tested_sha", "base_sha", "failed_burn", "failed_step", "error", "status", "reason", "superseded_by", "queued_at", "started_at", "finished_at", "created_at", "updated_at"},
		"step_results":              {"run_id", "burn", "step", "ordinal", "status", "exit_code", "log_start", "log_end", "started_at", "finished_at", "recorded_at"},
		"promotions":                {"sequence", "id", "repo_id", "source_branch", "source_sha", "target_ref", "previous_sha", "result_sha", "actor", "status", "run_id", "error", "created_at", "updated_at"},
		"issues":                    {"id", "repo_id", "kind", "branch", "title", "body", "state", "occurrences", "created_at", "updated_at", "closed_at", "ci_origin", "ci_work_sequence", "ci_work_id"},
		"issue_locks":               {"issue_id", "owner", "acquired_at", "expires_at"},
		"audit_actions":             {"id", "actor", "action", "resource_type", "resource_id", "details", "previous_sha256", "sha256", "created_at"},
		"audit_anchors":             {"id", "audit_id", "audit_sha256", "tsa_url", "receipt", "anchored_at", "created_at"},
		"token_credentials":         {"id", "name", "digest", "created_at", "last_used_at", "revoked_at", "activated_at"},
		"uplinks":                   {"id", "fingerprint", "identity", "token_credential_id", "auth_actor", "created_at", "updated_at"},
		"run_cancellations":         {"run_id", "job_name", "superseded_by", "reason", "created_at", "completed_at"},
		"receive_events":            {"id", "actor", "repo_id", "ref_kind", "ref", "old_sha", "object_sha", "commit_sha", "outcome", "run_id", "created_at"},
		"pending_token_credentials": {"token_credential_id", "created_at"},
		"publications":              {"sequence", "id", "repo_id", "run_id", "promotion_id", "ref_kind", "ref", "previous_sha", "previous_known", "result_sha", "actor", "status", "error", "created_at", "updated_at"},
		"ci_issue_work":             {"sequence", "repo_id", "branch", "origin", "run_id", "promotion_id", "actor", "created_at"},
		"ci_issue_projections":      {"repo_id", "branch", "origin", "work_sequence", "work_id", "outcome", "title", "body", "actor", "updated_at"},
	}
	for table, expected := range expectedColumns {
		actual := schemaTableColumns(t, store.db, table)
		if !slices.Equal(actual, expected) {
			t.Errorf("%s columns = %v, want %v", table, actual, expected)
		}
	}

	assertSchemaContains(t, store.db, "table", "runs",
		"status text not null check (status in ('queued', 'running', 'passed', 'failed', 'interrupted'))")
	assertSchemaContains(t, store.db, "table", "step_results",
		"status text not null check (status in ('passed', 'failed', 'skipped', 'timed_out'))")
	assertSchemaContains(t, store.db, "table", "promotions",
		"status text not null default 'pending' check (status in ('pending', 'passed', 'failed', 'interrupted'))")
	assertSchemaContains(t, store.db, "table", "run_cancellations",
		"superseded_by text not null default ''",
		"reason text not null check (reason in ('superseded', 'owner_restart'))")
	assertSchemaContains(t, store.db, "table", "publications",
		"status text not null default 'pending' check (status in ('pending', 'delivered', 'failed'))",
		"check (run_id is not null or promotion_id is not null)")
	assertSchemaContains(t, store.db, "table", "ci_issue_work",
		"origin = 'branch' and run_id is not null and promotion_id is null",
		"origin = 'promotion' and run_id is null and promotion_id is not null")

	foreignKeyRows, err := store.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if foreignKeyRows.Next() {
		_ = foreignKeyRows.Close()
		t.Fatal("fresh schema failed PRAGMA foreign_key_check")
	}
	if err := foreignKeyRows.Close(); err != nil {
		t.Fatal(err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenAdminClient(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("reopen fresh schema: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	assertFreshSchemaLedger(t, reopened.db)
}

func TestSchemaIdentityIsImmutable(t *testing.T) {
	t.Parallel()

	store, err := OpenAdminClient(context.Background(), filepath.Join(t.TempDir(), "oberth.sqlite"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	for name, statement := range map[string]string{
		"insert": `INSERT INTO schema_identity(singleton, identity) VALUES(2, 'other')`,
		"update": `UPDATE schema_identity SET identity = identity WHERE singleton = 1`,
		"delete": `DELETE FROM schema_identity WHERE singleton = 1`,
	} {
		if _, err := store.db.Exec(statement); err == nil || !strings.Contains(err.Error(), "schema identity is immutable") {
			t.Errorf("%s schema identity error = %v, want immutable rejection", name, err)
		}
	}
	assertFreshSchemaLedger(t, store.db)
}

func TestPromotionIssueWorkRequiresExclusivePromotionOwner(t *testing.T) {
	t.Parallel()

	store, err := OpenAdminClient(context.Background(), filepath.Join(t.TempDir(), "oberth.sqlite"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	if _, err := store.db.Exec(`
INSERT INTO upstreams(id, name, kind, base_url, created_at, updated_at)
VALUES(1, 'codeberg', 'forgejo', 'https://codeberg.org', 1, 1);
INSERT INTO repositories(id, name, upstream_id, default_branch, created_at, updated_at)
VALUES(1, 'oberth', 1, 'main', 1, 1);
INSERT INTO runs(id, repo_id, ref_kind, ref, sha, actor, trigger, status, queued_at, created_at, updated_at)
VALUES('run-1', 1, 'branch', 'feature/one', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'agent@host', 'push', 'passed', 1, 1, 1);
INSERT INTO promotions(id, repo_id, source_branch, source_sha, target_ref, previous_sha, result_sha, actor, run_id, created_at, updated_at)
VALUES('promotion-1', 1, 'feature/one', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'main', '', '', 'agent@host', '', 1, 1);`); err != nil {
		t.Fatal(err)
	}

	if _, err := store.db.Exec(`
INSERT INTO ci_issue_work(repo_id, branch, origin, run_id, promotion_id, actor, created_at)
VALUES(1, 'feature/one', 'promotion', 'run-1', 'promotion-1', 'agent@host', 1)`); err == nil {
		t.Fatal("promotion CI issue work accepted a run owner")
	}
	if _, err := store.db.Exec(`
INSERT INTO ci_issue_work(repo_id, branch, origin, promotion_id, actor, created_at)
VALUES(1, 'feature/one', 'promotion', 'promotion-1', 'agent@host', 1)`); err != nil {
		t.Fatalf("insert promotion CI issue work: %v", err)
	}
	var runIDIsNull bool
	if err := store.db.QueryRow(`
SELECT run_id IS NULL FROM ci_issue_work WHERE promotion_id = 'promotion-1'`).Scan(&runIDIsNull); err != nil {
		t.Fatal(err)
	}
	if !runIDIsNull {
		t.Fatal("promotion CI issue work stored a non-NULL run owner")
	}
}

func TestPromotionRunIndexAllowsOneNonEmptyBinding(t *testing.T) {
	t.Parallel()

	store, err := OpenAdminClient(context.Background(), filepath.Join(t.TempDir(), "oberth.sqlite"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, err := store.db.Exec(`
INSERT INTO upstreams(id, name, kind, base_url, created_at, updated_at)
VALUES(1, 'codeberg', 'forgejo', 'https://codeberg.org', 1, 1);
INSERT INTO repositories(id, name, upstream_id, default_branch, created_at, updated_at)
VALUES(1, 'oberth', 1, 'main', 1, 1);
INSERT INTO runs(id, repo_id, ref_kind, ref, sha, actor, trigger, status, queued_at, created_at, updated_at)
VALUES('run-1', 1, 'branch', 'feature/one', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'agent@host', 'promotion', 'passed', 1, 1, 1);`); err != nil {
		t.Fatal(err)
	}
	const insertPromotion = `
INSERT INTO promotions(id, repo_id, source_branch, source_sha, target_ref, previous_sha, result_sha, actor, run_id, created_at, updated_at)
VALUES(?, 1, 'feature/one', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'main', '', '', 'agent@host', ?, 1, 1)`
	if _, err := store.db.Exec(insertPromotion, "promotion-bound", "run-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(insertPromotion, "promotion-duplicate", "run-1"); err == nil {
		t.Fatal("second promotion accepted the same non-empty run binding")
	}
	for _, id := range []string{"promotion-unbound-1", "promotion-unbound-2"} {
		if _, err := store.db.Exec(insertPromotion, id, ""); err != nil {
			t.Fatalf("insert unbound promotion %s: %v", id, err)
		}
	}
}

func TestOpenRejectsIncompatibleVersionOneSchema(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name          string
		identitySetup string
	}{
		{name: "missing identity"},
		{
			name: "wrong identity",
			identitySetup: `
CREATE TABLE schema_identity(singleton INTEGER PRIMARY KEY, identity TEXT NOT NULL);
INSERT INTO schema_identity(singleton, identity) VALUES(1, 'wrong-schema-identity');`,
		},
	} {
		for _, opener := range []struct {
			name string
			open func(context.Context, string, Options) (*Store, error)
		}{
			{name: "owner", open: Open},
			{name: "admin", open: OpenAdminClient},
		} {
			t.Run(test.name+"/"+opener.name, func(t *testing.T) {
				t.Parallel()
				path := filepath.Join(t.TempDir(), "legacy.sqlite")
				database, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := database.Exec(`
CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
INSERT INTO schema_migrations(version, applied_at) VALUES(1, 0);` + test.identitySetup); err != nil {
					_ = database.Close()
					t.Fatal(err)
				}
				if err := database.Close(); err != nil {
					t.Fatal(err)
				}

				opened, err := opener.open(context.Background(), path, Options{})
				if opened != nil {
					_ = opened.Close()
				}
				if !errors.Is(err, ErrSchemaIncompatible) || !strings.Contains(err.Error(), "database schema is incompatible") {
					t.Fatalf("%s error = %v, want clear ErrSchemaIncompatible", opener.name, err)
				}
			})
		}
	}
}

func TestOpenRejectsLegacyAndFutureSchemaVersions(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		version int
	}{
		{name: "legacy migration chain", version: 7},
		{name: "future schema", version: 99},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "unsupported.sqlite")
			database, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`
CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
INSERT INTO schema_migrations(version, applied_at) VALUES(?, 0);`, test.version); err != nil {
				_ = database.Close()
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}

			opened, err := OpenAdminClient(context.Background(), path, Options{})
			if opened != nil {
				_ = opened.Close()
			}
			if !errors.Is(err, ErrSchemaTooNew) || !strings.Contains(err.Error(), "binary=2") {
				t.Fatalf("OpenAdminClient version %d error = %v, want clear ErrSchemaTooNew", test.version, err)
			}
		})
	}
}

func assertFreshSchemaLedger(t *testing.T, database *sql.DB) {
	t.Helper()

	var migrationRows, version int
	if err := database.QueryRow(`
SELECT count(*), COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&migrationRows, &version); err != nil {
		t.Fatal(err)
	}
	if migrationRows != len(migrations) || version != latestMigrationVersion {
		t.Fatalf("schema migration ledger rows=%d version=%d, want %d rows through version %d",
			migrationRows, version, len(migrations), latestMigrationVersion)
	}
	var identityRows int
	var identity string
	if err := database.QueryRow(`
SELECT count(*), COALESCE(MAX(identity), '') FROM schema_identity`).Scan(&identityRows, &identity); err != nil {
		t.Fatal(err)
	}
	if identityRows != 1 || identity != oberthSchemaIdentity {
		t.Fatalf("schema identity rows=%d identity=%q, want one %q row", identityRows, identity, oberthSchemaIdentity)
	}
}

func createExactLegacyV1Database(t *testing.T, path string) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`PRAGMA journal_mode = WAL;` + createMigrationLedger + `;` + migrations[0].sql + `
INSERT INTO schema_migrations(version, applied_at) VALUES(1, 1);`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertDatabaseFilesEqual(t *testing.T, got, want map[string][]byte, operation string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s changed SQLite file count from %d to %d", operation, len(want), len(got))
	}
	for suffix, expected := range want {
		if !bytes.Equal(got[suffix], expected) {
			t.Fatalf("%s changed SQLite file %q", operation, suffix)
		}
	}
}

func assertSchemaObjects(t *testing.T, database *sql.DB, objectType string, expected []string) {
	t.Helper()

	rows, err := database.Query(`
SELECT name FROM sqlite_schema
WHERE type = ? AND name NOT LIKE 'sqlite_%'
ORDER BY name`, objectType)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var actual []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		actual = append(actual, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(actual, expected) {
		t.Errorf("%s objects = %v, want %v", objectType, actual, expected)
	}
}

func schemaTableColumns(t *testing.T, database *sql.DB, table string) []string {
	t.Helper()

	rows, err := database.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}

func assertSchemaContains(t *testing.T, database *sql.DB, objectType, name string, fragments ...string) {
	t.Helper()

	var definition string
	if err := database.QueryRow(`
SELECT sql FROM sqlite_schema WHERE type = ? AND name = ?`, objectType, name).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	definition = strings.ToLower(strings.Join(strings.Fields(definition), " "))
	for _, fragment := range fragments {
		if !strings.Contains(definition, fragment) {
			t.Errorf("%s %s does not contain %q: %s", objectType, name, fragment, definition)
		}
	}
}
