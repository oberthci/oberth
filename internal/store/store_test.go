package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func testStore(t *testing.T, now *time.Time) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oberth.db")
	s, err := Open(context.Background(), path, Options{Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}

func readAuthoritativeDatabaseFiles(t *testing.T, path string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		body, err := os.ReadFile(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("read database file %q: %v", path+suffix, err)
		}
		files[suffix] = body
	}
	return files
}

func createRepo(t *testing.T, s *Store) model.Repository {
	t.Helper()
	upstream, err := s.CreateUpstream(context.Background(), model.UpstreamSpec{
		Name: "codeberg", Kind: "forgejo", BaseURL: "https://codeberg.org",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.CreateRepository(context.Background(), model.RepositorySpec{
		Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func testRunSpec(repoID int64, ref, sha string) model.RunSpec {
	return model.RunSpec{
		RepoID: repoID, RefKind: model.RefBranch, Ref: ref, SHA: sha,
		Actor: "SHA256:test", Trigger: "push",
	}
}

func TestOpenAppliesMigrationsAndSQLitePragmas(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)

	var version int
	if err := s.db.QueryRowContext(context.Background(), `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != latestMigrationVersion {
		t.Fatalf("migration version = %d, want %d", version, latestMigrationVersion)
	}
	var journal string
	if err := s.db.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Fatalf("journal mode = %q, want wal", journal)
	}
	var timeout int
	if err := s.db.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != 5000 {
		t.Fatalf("busy timeout = %d, want 5000", timeout)
	}
}

func TestStartupOpenersRequireCurrentSchemaWithoutMutatingIt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := CreateGenesis(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test", ResourceType: "startup", ResourceID: "current",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	inspection, err := InspectCurrent(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inspection.VerifyAuditState(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := inspection.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "write", ResourceType: "startup", ResourceID: "rejected",
	}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Fatalf("read-only inspection write error = %v", err)
	}
	if err := inspection.Close(); err != nil {
		t.Fatal(err)
	}

	current, err := OpenCurrent(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("startup inspection/current open changed the SQLite database bytes")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("genesis database mode = %o, want 600", info.Mode().Perm())
	}
	if reopened, err := CreateGenesis(ctx, path, Options{}); err == nil {
		_ = reopened.Close()
		t.Fatal("genesis creation replaced an existing database")
	}
}

func TestStartupOpenersRefuseVersionOneWithoutMigrating(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth-v1.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if _, err := database.Exec(`
CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
` + migrations[0].sql + `
INSERT INTO schema_migrations(version, applied_at) VALUES(1, 1);`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	before := readAuthoritativeDatabaseFiles(t, path)
	for _, opener := range []struct {
		name string
		open func(context.Context, string, Options) (*Store, error)
	}{
		{name: "inspection", open: InspectCurrent},
		{name: "current", open: OpenCurrent},
		{name: "owner", open: Open},
		{name: "admin", open: OpenAdminClient},
	} {
		t.Run(opener.name, func(t *testing.T) {
			opened, err := opener.open(ctx, path, Options{})
			if opened != nil {
				_ = opened.Close()
			}
			if !errors.Is(err, ErrSchemaLegacyV1) {
				t.Fatalf("startup open error = %v, want exact legacy-schema rejection", err)
			}
			after := readAuthoritativeDatabaseFiles(t, path)
			if len(after) != len(before) {
				t.Fatalf("startup opener changed authoritative SQLite file count from %d to %d", len(before), len(after))
			}
			for suffix, expected := range before {
				if !bytes.Equal(after[suffix], expected) {
					t.Fatalf("startup opener migrated or otherwise changed SQLite file %q", suffix)
				}
			}
		})
	}
}

func TestLegacyMigrationRefusesCurrentSchemaWithoutChangingIt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := CreateGenesis(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	head, err := database.VerifyAuditState(ctx)
	if err != nil {
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
	if !errors.Is(err, ErrSchemaIncompatible) || !strings.Contains(err.Error(), "expected schema version 1, found 12") {
		t.Fatalf("current-schema legacy migration error = %v", err)
	}
	after := readAuthoritativeDatabaseFiles(t, path)
	for suffix, expected := range before {
		if actual, ok := after[suffix]; !ok || !bytes.Equal(actual, expected) {
			t.Fatalf("authoritative SQLite file %q changed during refused legacy migration", suffix)
		}
	}
}

func TestStartupInspectionVerifiesNonemptyWALWithoutChangingIt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := CreateGenesis(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.db.ExecContext(ctx, `PRAGMA wal_autocheckpoint = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test", ResourceType: "startup", ResourceID: "wal",
	}); err != nil {
		t.Fatal(err)
	}
	walBefore, err := os.ReadFile(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if len(walBefore) == 0 {
		t.Fatal("test did not create a nonempty SQLite WAL")
	}
	shmBefore, err := os.ReadFile(path + "-shm")
	if err != nil {
		t.Fatal(err)
	}

	inspection, err := InspectCurrent(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	head, err := inspection.VerifyAuditState(ctx)
	if err != nil {
		_ = inspection.Close()
		t.Fatal(err)
	}
	if head.ID != 1 {
		_ = inspection.Close()
		t.Fatalf("read-only WAL audit head = %d, want 1", head.ID)
	}
	if err := inspection.Close(); err != nil {
		t.Fatal(err)
	}
	walAfter, err := os.ReadFile(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(walAfter, walBefore) {
		t.Fatal("read-only startup inspection changed committed WAL bytes")
	}
	shmAfter, err := os.ReadFile(path + "-shm")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(shmAfter, shmBefore) {
		t.Fatal("read-only startup inspection changed the source WAL index")
	}
}

func TestStartupInspectionKeepsCrashSnapshotWithoutSHMUntouched(t *testing.T) {
	ctx := context.Background()
	sourcePath := filepath.Join(t.TempDir(), "source.sqlite")
	database, err := CreateGenesis(ctx, sourcePath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `PRAGMA wal_autocheckpoint = 0`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test", ResourceType: "startup", ResourceID: "crash-wal",
	}); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	mainBefore, err := os.ReadFile(sourcePath)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	walBefore, err := os.ReadFile(sourcePath + "-wal")
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	crashPath := filepath.Join(t.TempDir(), "oberth.sqlite")
	if err := os.WriteFile(crashPath, mainBefore, 0o600); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := os.WriteFile(crashPath+"-wal", walBefore, 0o600); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(crashPath + "-shm"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crash fixture unexpectedly has a WAL index: %v", err)
	}

	inspection, err := InspectCurrent(ctx, crashPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	head, err := inspection.VerifyAuditState(ctx)
	if err != nil {
		_ = inspection.Close()
		t.Fatal(err)
	}
	if head.ID != 1 {
		_ = inspection.Close()
		t.Fatalf("crash WAL audit head = %d, want 1", head.ID)
	}
	if err := inspection.Close(); err != nil {
		t.Fatal(err)
	}
	mainAfter, err := os.ReadFile(crashPath)
	if err != nil {
		t.Fatal(err)
	}
	walAfter, err := os.ReadFile(crashPath + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mainAfter, mainBefore) || !bytes.Equal(walAfter, walBefore) {
		t.Fatal("read-only startup inspection changed crash-snapshot database or WAL bytes")
	}
	if _, err := os.Lstat(crashPath + "-shm"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only startup inspection created a source WAL index: %v", err)
	}
}

func TestOpenRejectsNewerSchemaBeforeRecoveryWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
INSERT INTO schema_migrations(version, applied_at) VALUES(?, 0);
CREATE TABLE runs(status TEXT NOT NULL, phase TEXT NOT NULL, reason TEXT NOT NULL, error TEXT NOT NULL, finished_at INTEGER, updated_at INTEGER);
INSERT INTO runs(status, phase, reason, error) VALUES('running', 'running', '', '');`, latestMigrationVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path, Options{}); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Open error = %v, want ErrSchemaTooNew", err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var status string
	if err := db.QueryRow(`SELECT status FROM runs`).Scan(&status); err != nil || status != "running" {
		t.Fatalf("status after rejected open = %q, %v", status, err)
	}
}

func TestRunFIFOClaimSupersedeAndResolution(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	first, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/a", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	second, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/b", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != first.ID || claimed.Status != model.RunRunning {
		t.Fatalf("first claim = %#v, want run %s running", claimed, first.ID)
	}

	now = now.Add(time.Second)
	replacement, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/a", "cccccccccccccccccccccccccccccccccccccccc"))
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := s.Run(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != model.RunInterrupted || interrupted.Phase != "interrupted" ||
		interrupted.Reason != "superseded by newer run" || interrupted.SupersededBy != replacement.ID || interrupted.FinishedAt == nil {
		t.Fatalf("interrupted superseded run = %#v", interrupted)
	}

	next, err := s.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != second.ID {
		t.Fatalf("second claim = %s, want FIFO run %s", next.ID, second.ID)
	}
	byRef, err := s.LatestRunForRef(ctx, repo.ID, "feature/a")
	if err != nil || byRef.ID != replacement.ID {
		t.Fatalf("latest by ref = %#v, %v", byRef, err)
	}
	bySHA, err := s.LatestRunForSHA(ctx, repo.ID, replacement.SHA)
	if err != nil || bySHA.ID != replacement.ID {
		t.Fatalf("latest by SHA = %#v, %v", bySHA, err)
	}
}

func TestRunLifecycleListingAndFlexibleResolution(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	sha := "abc1234aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	base := "1111111111111111111111111111111111111111"
	spec := testRunSpec(repo.ID, "release/one", sha)
	spec.Release = true
	spec.Trigger = "manual"
	spec.BaseSHA = base
	run, err := s.EnqueueRun(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Release || run.Actor != spec.Actor || run.Trigger != "manual" || run.TestedSHA != sha || run.BaseSHA != base {
		t.Fatalf("enqueued run metadata = %#v", run)
	}
	if _, err := s.SetRunJobName(ctx, run.ID, "oberth-job-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	finished, err := s.FinishRun(ctx, run.ID, model.RunResult{
		Status: model.RunFailed, Phase: "test", FailedBurn: "test",
		FailedStep: "unit", Error: "tests failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished.JobName != "oberth-job-1" || finished.Status != model.RunFailed ||
		finished.Phase != "test" || finished.FailedBurn != "test" || finished.FailedStep != "unit" ||
		finished.Error != "tests failed" || finished.FinishedAt == nil {
		t.Fatalf("terminal run = %#v", finished)
	}
	if _, err := s.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second terminal result error = %v, want ErrInvalidState", err)
	}

	byBranch, err := s.ResolveRun(ctx, repo.ID, "release/one")
	if err != nil || byBranch.ID != run.ID {
		t.Fatalf("resolve branch = %#v, %v", byBranch, err)
	}
	byShortSHA, err := s.ResolveRun(ctx, repo.ID, "abc1234")
	if err != nil || byShortSHA.ID != run.ID {
		t.Fatalf("resolve short SHA = %#v, %v", byShortSHA, err)
	}
	byFullSHA, err := s.ResolveRun(ctx, repo.ID, sha)
	if err != nil || byFullSHA.ID != run.ID {
		t.Fatalf("resolve full SHA = %#v, %v", byFullSHA, err)
	}

	other := testRunSpec(repo.ID, "ambiguous", "abc1234bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if _, err := s.EnqueueRun(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveRun(ctx, repo.ID, "abc1234"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous short SHA error = %v, want ErrAmbiguous", err)
	}
	runs, err := s.ListRecentRuns(ctx, model.RunListFilter{RepoID: repo.ID, Limit: 1})
	if err != nil || len(runs) != 1 || runs[0].SHA != other.SHA {
		t.Fatalf("recent runs = %#v, %v", runs, err)
	}
	repositories, err := s.ListRepositories(ctx)
	if err != nil || len(repositories) != 1 || repositories[0].ID != repo.ID {
		t.Fatalf("repositories = %#v, %v", repositories, err)
	}
}

func TestListRecentRunsFiltersByRef(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	branchA := "feature/alpha"
	branchB := "feature/beta"
	shaA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	if _, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, branchA, shaA)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, branchB, shaB)); err != nil {
		t.Fatal(err)
	}

	// Unfiltered: both runs returned.
	all, err := s.ListRecentRuns(ctx, model.RunListFilter{RepoID: repo.ID, Limit: 10})
	if err != nil || len(all) != 2 {
		t.Fatalf("unfiltered runs = %d, %v", len(all), err)
	}

	// Filter by branchA: only branchA's run.
	onlyA, err := s.ListRecentRuns(ctx, model.RunListFilter{RepoID: repo.ID, Ref: branchA, Limit: 10})
	if err != nil || len(onlyA) != 1 || onlyA[0].Ref != branchA {
		t.Fatalf("ref=%q runs = %#v, %v", branchA, onlyA, err)
	}

	// Filter by branchB: only branchB's run.
	onlyB, err := s.ListRecentRuns(ctx, model.RunListFilter{RepoID: repo.ID, Ref: branchB, Limit: 10})
	if err != nil || len(onlyB) != 1 || onlyB[0].Ref != branchB {
		t.Fatalf("ref=%q runs = %#v, %v", branchB, onlyB, err)
	}

	// Filter by nonexistent ref: empty result.
	none, err := s.ListRecentRuns(ctx, model.RunListFilter{RepoID: repo.ID, Ref: "nonexistent", Limit: 10})
	if err != nil || len(none) != 0 {
		t.Fatalf("ref=nonexistent runs = %#v, %v", none, err)
	}
}

func TestRestartRecoveryInterruptsRunningRuns(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/restart", "dddddddddddddddddddddddddddddddddddddddd"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunJobName(ctx, run.ID, "job-before-restart"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Minute)
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	recovered, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Runs with a known job_name stay in "running" after store recovery so the
	// scheduler's startup reconciliation can query K8s for their terminal state.
	if recovered.Status != model.RunRunning {
		t.Fatalf("recovered run status = %q, want running (scheduler reconciles)", recovered.Status)
	}
	cancellations, err := s.PendingRunCancellations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cancellations) != 1 || cancellations[0].RunID != run.ID || cancellations[0].JobName != "job-before-restart" || cancellations[0].Reason != "owner_restart" {
		t.Fatalf("restart cancellations = %#v", cancellations)
	}
	// A claimed ordinary branch run without a job_name is requeued by store
	// recovery (issue #270): the old attempt is interrupted with a link to a
	// fresh queued copy, and the no-Job cancellation obligation terminalizes
	// inside the recovery transaction because owner startup proves the
	// claiming worker is gone.
	noJobRun, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/no-job", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	noJobRecovered, err := s.Run(ctx, noJobRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if noJobRecovered.Status != model.RunInterrupted || noJobRecovered.FinishedAt == nil || noJobRecovered.SupersededBy == "" {
		t.Fatalf("no-job recovered run = %#v, want interrupted and superseded by its requeue", noJobRecovered)
	}
	requeued, err := s.Run(ctx, noJobRecovered.SupersededBy)
	if err != nil {
		t.Fatal(err)
	}
	if requeued.Status != model.RunQueued || requeued.Ref != noJobRun.Ref || requeued.SHA != noJobRun.SHA ||
		requeued.Trigger != noJobRun.Trigger || requeued.Actor != noJobRun.Actor {
		t.Fatalf("requeued run = %#v, want queued copy of %#v", requeued, noJobRun)
	}
	// The JobName-carrying run from the first phase still owns its pending
	// owner_restart obligation; the requeued no-Job run must not add one.
	cancellations, err = s.PendingRunCancellations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cancellations) != 1 || cancellations[0].RunID != run.ID {
		t.Fatalf("pending cancellations after requeue = %#v, want only the Job-carrying run", cancellations)
	}
}

// TestRestartRecoveryDoesNotRequeueTagRun pins the conservative side of the
// issue #270 requeue: release-tier (tag) work is never re-fired
// automatically; a claimed tag run without a Job is terminally interrupted
// exactly as before.
func TestRestartRecoveryDoesNotRequeueTagRun(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 6, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefTag, Ref: "v1.0.0",
		SHA: "cccccccccccccccccccccccccccccccccccccccc", Actor: "agent@host", Trigger: "tag",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	recovered, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.RunInterrupted || recovered.SupersededBy != "" || recovered.Reason != "oberth restarted" {
		t.Fatalf("recovered tag run = %#v, want terminally interrupted without requeue", recovered)
	}
}

// TestRequeueStrandedRunReplacesOwnerRestartObligation covers the scheduler
// reconciliation path of issue #270 at the store level: a stranded run whose
// owner_restart cancellation already exists (run_id is the primary key) gets
// its obligation upserted to a superseded one pointing at the requeued run,
// re-armed for the cancellation pass.
func TestRequeueStrandedRunReplacesOwnerRestartObligation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 6, 30, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/stranded", "abcabcabcabcabcabcabcabcabcabcabcabcabca"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunJobName(ctx, run.ID, "job-stranded"); err != nil {
		t.Fatal(err)
	}
	// A credentialed (ci-secrets tier) branch run is requeue-eligible; the
	// copy re-earns credentials through fresh scheduler admission.
	if err := s.SetRunCredentialed(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	requeued, err := s.RequeueStrandedRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != model.RunInterrupted || old.SupersededBy != requeued.ID {
		t.Fatalf("stranded run after requeue = %#v, want interrupted superseded by %s", old, requeued.ID)
	}
	if requeued.Status != model.RunQueued || requeued.Ref != run.Ref || requeued.SHA != run.SHA || requeued.Credentialed {
		t.Fatalf("requeued run = %#v, want uncredentialed queued copy", requeued)
	}
	pending, err := s.PendingRunCancellations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].RunID != run.ID || pending[0].JobName != "job-stranded" ||
		pending[0].Reason != "superseded" || pending[0].SupersededBy != requeued.ID {
		t.Fatalf("pending obligations = %#v, want upserted superseded obligation for job-stranded", pending)
	}

	// A second requeue attempt is refused: the run is no longer running.
	if _, err := s.RequeueStrandedRun(ctx, run.ID); !errors.Is(err, ErrRequeueIneligible) {
		t.Fatalf("second requeue error = %v, want ErrRequeueIneligible", err)
	}
}

func TestAdminClientOpenDoesNotRecoverOwnerRuns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	owner, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := owner.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := owner.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	enqueued, err := owner.EnqueueRun(ctx, testRunSpec(repository.ID, "feature/admin-open", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}

	admin, err := OpenAdminClient(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	run, err := admin.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunRunning {
		t.Fatalf("admin open changed active run status to %q", run.Status)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStepResultsRetainLogByteOffsets(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(context.Background(), testRunSpec(repo.ID, "feature/logs", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.PutStepResult(context.Background(), model.StepResult{
		RunID: run.ID, Burn: "test", Step: "unit", Ordinal: 2,
		Status: model.StepTimedOut, ExitCode: -1, LogStart: 128, LogEnd: 4096,
		DeclaredSize: "L", MaxRSSBytes: 64 << 20, UserCPU: 1250 * time.Millisecond, SystemCPU: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LogStart != 128 || result.LogEnd != 4096 || result.DeclaredSize != "L" ||
		result.MaxRSSBytes != 64<<20 || result.UserCPU != 1250*time.Millisecond ||
		result.SystemCPU != 250*time.Millisecond || result.RecordedAt.IsZero() {
		t.Fatalf("step result = %#v", result)
	}
	steps, err := s.StepResults(context.Background(), run.ID)
	if err != nil || len(steps) != 1 || steps[0].Step != "unit" || steps[0].Status != model.StepTimedOut ||
		steps[0].DeclaredSize != "L" || steps[0].MaxRSSBytes != 64<<20 ||
		steps[0].UserCPU != 1250*time.Millisecond || steps[0].SystemCPU != 250*time.Millisecond {
		t.Fatalf("steps = %#v, %v", steps, err)
	}
}

// TestPutStepResultRefusesEveryNonTerminalStatus is the guard that keeps a
// plan out of the results table.
//
// step_results is the durable record of what a run *did*, and its CHECK
// constraint admits only terminal statuses. A seeded plan entry is by
// definition not one, so the plan lives beside the run log instead and this
// guard stays exactly as strict as it was: a half-finished run can never be
// recorded as a result, and a pending row can never reach the store to be
// mistaken for one.
func TestPutStepResultRefusesEveryNonTerminalStatus(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(context.Background(), testRunSpec(repo.ID, "feature/pending", "acacacacacacacacacacacacacacacacacacacac"))
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []model.StepStatus{model.StepPending, model.StepQueued, model.StepRunning, "invented"} {
		if _, err := s.PutStepResult(context.Background(), model.StepResult{
			RunID: run.ID, Burn: "setup", Step: "tools-dir", Ordinal: 0,
			Status: status, DeclaredSize: "M",
		}); !errors.Is(err, ErrInvalid) {
			t.Errorf("PutStepResult(%q) error = %v, want ErrInvalid", status, err)
		}
	}
	steps, err := s.StepResults(context.Background(), run.ID)
	if err != nil || len(steps) != 0 {
		t.Fatalf("steps = %#v, %v; want none recorded", steps, err)
	}
}

func TestPromotionLifecycleIsPendingThenImmutable(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	promotion, err := s.AppendPromotion(context.Background(), model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/fab", SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetRef: "main", PreviousSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ResultSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "SHA256:operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if promotion.ID == "" || promotion.Status != model.PromotionPending || promotion.CreatedAt.IsZero() || promotion.UpdatedAt.IsZero() {
		t.Fatalf("pending promotion = %#v", promotion)
	}
	now = now.Add(time.Second)
	finished, err := s.FinishPromotion(context.Background(), promotion.ID, model.PromotionFailed, "run-1", "tests failed")
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != model.PromotionFailed || finished.RunID != "run-1" || !finished.UpdatedAt.Equal(now) {
		t.Fatalf("finished promotion = %#v", finished)
	}
	if _, err := s.FinishPromotion(context.Background(), promotion.ID, model.PromotionFailed, "run-2", "late"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second terminal transition error = %v, want ErrInvalidState", err)
	}
	if _, err := s.db.ExecContext(context.Background(), `UPDATE promotions SET actor = 'tampered' WHERE id = ?`, promotion.ID); err == nil {
		t.Fatal("terminal promotion mutation unexpectedly succeeded")
	}
	if _, err := s.db.ExecContext(context.Background(), `DELETE FROM promotions WHERE id = ?`, promotion.ID); err == nil {
		t.Fatal("promotion delete unexpectedly succeeded")
	}
}

func TestPromotionRunAdmissionIsAtomicAndOutsideBranchSupersession(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	const (
		base   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		merged = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		ref    = "promotion/main/bbbbbbbbbbbb"
	)
	runSpec := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: ref, SHA: merged,
		Actor: "agent@host", Trigger: "promotion", TestedSHA: merged, BaseSHA: base,
	}
	promotionSpec := model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/one", SourceSHA: merged,
		TargetRef: "main", PreviousSHA: base, ResultSHA: merged, Actor: "agent@host",
	}
	enqueued, promotion, err := s.EnqueuePromotionRun(ctx, runSpec, promotionSpec)
	if err != nil {
		t.Fatal(err)
	}
	if promotion.RunID != enqueued.ID || promotion.Status != model.PromotionPending {
		t.Fatalf("atomic promotion/run = %#v / %#v", promotion, enqueued.Run)
	}
	byRun, err := s.PromotionByRun(ctx, enqueued.ID)
	if err != nil || byRun.ID != promotion.ID {
		t.Fatalf("promotion by run = %#v, %v", byRun, err)
	}

	branch := testRunSpec(repo.ID, ref, "cccccccccccccccccccccccccccccccccccccccc")
	if _, err := s.EnqueueRun(ctx, branch); err != nil {
		t.Fatal(err)
	}
	unchanged, err := s.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != model.RunQueued {
		t.Fatalf("branch push superseded promotion run: %#v", unchanged)
	}
}

func TestPlanPromotionRecordsAuditAtomically(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	const (
		source   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		previous = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		result   = "cccccccccccccccccccccccccccccccccccccccc"
	)
	promotion, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/plan", SourceSHA: source,
		TargetRef: "main", Actor: "SHA256:operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if promotion.PreviousSHA != "" || promotion.ResultSHA != "" {
		t.Fatalf("new promotion should have empty plan, got previous=%q result=%q", promotion.PreviousSHA, promotion.ResultSHA)
	}
	var auditBefore int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_actions WHERE action = 'promotion.pending'`).Scan(&auditBefore); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	planned, err := s.PlanPromotion(ctx, promotion.ID, previous, result)
	if err != nil {
		t.Fatal(err)
	}
	if planned.PreviousSHA != previous || planned.ResultSHA != result {
		t.Fatalf("planned promotion = %#v", planned)
	}
	var auditAfter int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_actions WHERE action = 'promotion.pending'`).Scan(&auditAfter); err != nil {
		t.Fatal(err)
	}
	if auditAfter != auditBefore+1 {
		t.Fatalf("expected exactly one promotion.pending audit action, got %d new", auditAfter-auditBefore)
	}
	var details string
	if err := s.db.QueryRow(`SELECT details FROM audit_actions WHERE action = 'promotion.pending' ORDER BY id DESC LIMIT 1`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, result) || !strings.Contains(details, previous) {
		t.Fatalf("audit details should contain plan SHAs, got %s", details)
	}
	idempotent, err := s.PlanPromotion(ctx, promotion.ID, previous, result)
	if err != nil {
		t.Fatalf("idempotent plan = %v", err)
	}
	if idempotent.ID != planned.ID {
		t.Fatalf("idempotent plan returned a different promotion")
	}
}

func TestPromotionRunAdmissionRollsBackBothRecords(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER reject_test_promotion BEFORE INSERT ON promotions BEGIN SELECT RAISE(ABORT, 'promotion unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	const (
		base   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		merged = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	_, _, err := s.EnqueuePromotionRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/bbbbbbbbbbbb", SHA: merged,
		Actor: "agent@host", Trigger: "promotion", TestedSHA: merged, BaseSHA: base,
	}, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/one", SourceSHA: merged,
		TargetRef: "main", PreviousSHA: base, ResultSHA: merged, Actor: "agent@host",
	})
	if err == nil {
		t.Fatal("promotion admission unexpectedly succeeded")
	}
	var runs, promotions int
	if err := s.db.QueryRow(`SELECT count(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM promotions`).Scan(&promotions); err != nil {
		t.Fatal(err)
	}
	if runs != 0 || promotions != 0 {
		t.Fatalf("partially committed promotion admission: runs=%d promotions=%d", runs, promotions)
	}
}

func TestCIIssueDedupeAndAutoClose(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	first, err := s.UpsertCIIssue(ctx, "runner@oberth", repo.ID, "feature/red", "CI failed", "first failure")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireIssueLock(ctx, first.ID, "monitor@oberth"); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("auto-locked CI issue acquire error = %v, want ErrLockHeld", err)
	}
	now = now.Add(time.Minute)
	second, err := s.UpsertCIIssue(ctx, "runner@oberth", repo.ID, "feature/red", "CI still failed", "second failure")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Occurrences != 2 || second.Body != "second failure" {
		t.Fatalf("deduplicated issue = %#v, first = %#v", second, first)
	}
	closed, err := s.CloseCIIssue(ctx, "runner@oberth", first.ID, "resolved by abc (run run-1)")
	if err != nil {
		t.Fatal(err)
	}
	if closed.State != model.IssueClosed || closed.ClosedAt == nil || !strings.Contains(closed.Body, "resolved by abc (run run-1)") {
		t.Fatalf("closed issue = %#v", closed)
	}
	third, err := s.UpsertCIIssue(ctx, "runner@oberth", repo.ID, "feature/red", "CI regressed", "third failure")
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID || third.Occurrences != 1 {
		t.Fatalf("new CI incident = %#v", third)
	}
	if _, err := s.CloseCIIssue(ctx, "runner@oberth", first.ID, "stale close"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale CI close error = %v, want ErrNotFound", err)
	}
	stillOpen, err := s.OpenCIIssue(ctx, repo.ID, "feature/red")
	if err != nil {
		t.Fatal(err)
	}
	if stillOpen.ID != third.ID || stillOpen.State != model.IssueOpen {
		t.Fatalf("replacement CI issue = %#v, want open issue %d", stillOpen, third.ID)
	}
}

func TestManualIssueCRUDAndPagination(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	var created []model.Issue
	for _, title := range []string{"one", "two", "three"} {
		issue, err := s.CreateManualIssue(ctx, "agent@host", model.ManualIssueSpec{Title: title, Body: title + " body"})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, issue)
		now = now.Add(time.Second)
	}
	if _, err := s.UpsertCIIssue(ctx, "runner@oberth", repo.ID, "feature/red", "CI failed", "failure"); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListIssues(ctx, model.IssueListFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Issues) != 2 || page.Issues[0].Kind != model.IssueCI || page.Issues[1].Title != "three" || page.NextBefore == 0 {
		t.Fatalf("first page = %#v", page)
	}
	next, err := s.ListIssues(ctx, model.IssueListFilter{Kind: model.IssueManual, Limit: 2, BeforeID: page.NextBefore})
	if err != nil || len(next.Issues) != 2 {
		t.Fatalf("second page = %#v, %v", next, err)
	}
	newTitle := "updated"
	closed := model.IssueClosed
	updated, err := s.UpdateManualIssue(ctx, "agent@host", created[0].ID, model.IssuePatch{Title: &newTitle, State: &closed})
	if err != nil || updated.Title != newTitle || updated.ClosedAt == nil {
		t.Fatalf("updated issue = %#v, %v", updated, err)
	}
	if err := s.DeleteManualIssue(ctx, "agent@host", created[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Issue(ctx, created[1].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted issue lookup error = %v, want ErrNotFound", err)
	}
	var attributed int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_actions WHERE resource_type = 'issue' AND actor = 'agent@host'`).Scan(&attributed); err != nil || attributed != 5 {
		t.Fatalf("attributed manual issue actions = %d, %v", attributed, err)
	}
}

func TestGlobalManualIssueCRUDAndFiltering(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	global, err := s.CreateManualIssue(ctx, "agent@host", model.ManualIssueSpec{
		Title: "global follow-up",
		Body:  "applies across repositories",
	})
	if err != nil {
		t.Fatal(err)
	}
	if global.RepoID != 0 {
		t.Fatalf("global issue repository = %d, want 0", global.RepoID)
	}
	var storedRepoID sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT repo_id FROM issues WHERE id = ?`, global.ID).Scan(&storedRepoID); err != nil {
		t.Fatal(err)
	}
	if storedRepoID.Valid {
		t.Fatalf("stored global issue repository = %d, want NULL", storedRepoID.Int64)
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO issues(repo_id, kind, title, body, state, occurrences, created_at, updated_at)
VALUES(?, 'manual', 'scoped', '', 'open', 1, ?, ?)`, repo.ID, unixNano(now), unixNano(now)); err == nil {
		t.Fatal("schema accepted a repository-scoped manual issue")
	}

	loaded, err := s.Issue(ctx, global.ID)
	if err != nil || loaded.RepoID != 0 {
		t.Fatalf("loaded global issue = %#v, %v", loaded, err)
	}
	updatedTitle := "updated global follow-up"
	updated, err := s.UpdateManualIssue(ctx, "agent@host", global.ID, model.IssuePatch{Title: &updatedTitle})
	if err != nil || updated.RepoID != 0 || updated.Title != updatedTitle {
		t.Fatalf("updated global issue = %#v, %v", updated, err)
	}

	second, err := s.CreateManualIssue(ctx, "agent@host", model.ManualIssueSpec{
		Title: "second global follow-up",
	})
	if err != nil {
		t.Fatal(err)
	}
	all, err := s.ListIssues(ctx, model.IssueListFilter{Kind: model.IssueManual, Limit: 10})
	if err != nil || len(all.Issues) != 2 || all.Issues[0].ID != second.ID || all.Issues[1].ID != global.ID {
		t.Fatalf("all manual issues = %#v, %v", all, err)
	}
	repositoryOnly, err := s.ListIssues(ctx, model.IssueListFilter{RepoID: repo.ID, Kind: model.IssueManual, Limit: 10})
	if err != nil || len(repositoryOnly.Issues) != 0 {
		t.Fatalf("repository filter included global manual issues = %#v, %v", repositoryOnly, err)
	}
}

func TestIssueMutationRollsBackWhenAuditInsertFails(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER reject_issue_audit BEFORE INSERT ON audit_actions WHEN NEW.resource_type = 'issue' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateManualIssue(ctx, "agent@host", model.ManualIssueSpec{Title: "must rollback"}); err == nil {
		t.Fatal("issue creation unexpectedly succeeded")
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM issues`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("issues after audit failure = %d, %v", count, err)
	}
}

func TestManualIssueCloseRollsBackLockDeletionWhenAuditFails(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	issue, err := s.CreateManualIssue(ctx, "agent@host", model.ManualIssueSpec{Title: "must stay open"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireIssueLock(ctx, issue.ID, "agent@host"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`
CREATE TRIGGER reject_issue_close_audit
BEFORE INSERT ON audit_actions WHEN NEW.action = 'issue.update'
BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}

	closed := model.IssueClosed
	if _, err := s.UpdateManualIssue(ctx, "agent@host", issue.ID, model.IssuePatch{State: &closed}); err == nil {
		t.Fatal("manual issue close unexpectedly succeeded")
	}
	stored, err := s.Issue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != model.IssueOpen || stored.ClosedAt != nil {
		t.Fatalf("issue after rolled-back close = %#v", stored)
	}
	if _, err := s.RenewIssueLock(ctx, issue.ID, "agent@host"); err != nil {
		t.Fatalf("lock was not restored with rolled-back close: %v", err)
	}
}

func TestSupersededRunCancellationPersistsAndLateJobNameIsRetained(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	first, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/cancel", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/cancel", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	if len(replacement.Cancellations) != 1 || replacement.Cancellations[0].RunID != first.ID || replacement.Cancellations[0].JobName != "" {
		t.Fatalf("enqueue cancellations = %#v", replacement.Cancellations)
	}
	if _, err := s.SetRunJobName(ctx, first.ID, "job-late"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	pending, err := s.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 1 || pending[0].JobName != "job-late" || pending[0].SupersededBy != replacement.ID {
		t.Fatalf("pending cancellations = %#v, %v", pending, err)
	}
	if err := s.CompleteRunCancellation(ctx, first.ID, "job-late"); err != nil {
		t.Fatal(err)
	}
	pending, err = s.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after completion = %#v, %v", pending, err)
	}
}

func TestEnqueueReturnsKnownJobCancellation(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	first, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/known-job", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunJobName(ctx, first.ID, "job-known"); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/known-job", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	if len(replacement.Cancellations) != 1 || replacement.Cancellations[0].RunID != first.ID || replacement.Cancellations[0].JobName != "job-known" {
		t.Fatalf("known job cancellation = %#v", replacement.Cancellations)
	}
}

func TestConcurrentStoreClientsDeduplicateReceivesAndSerializeEnqueue(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	first, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	upstream, err := first.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := first.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenAdminClient(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	record := model.ReceiveEventSpec{
		ID: "receive-delete", Actor: "agent@host", RepoID: repository.ID,
		RefKind: model.RefBranch, Ref: "feature/deleted", Outcome: "deleted",
	}
	type recordResult struct {
		duplicate bool
		err       error
	}
	records := make(chan recordResult, 2)
	start := make(chan struct{})
	for _, client := range []*Store{first, second} {
		go func(client *Store) {
			<-start
			duplicate, err := client.RecordReceiveEvent(ctx, record)
			records <- recordResult{duplicate: duplicate, err: err}
		}(client)
	}
	close(start)
	duplicates := 0
	for range 2 {
		result := <-records
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.duplicate {
			duplicates++
		}
	}
	if duplicates != 1 {
		t.Fatalf("record duplicate results = %d, want one", duplicates)
	}

	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	event := model.ReceiveEventSpec{
		ID: "receive-enqueue", Actor: "agent@host", RepoID: repository.ID,
		RefKind: model.RefBranch, Ref: "feature/replay", ObjectSHA: sha, CommitSHA: sha, Outcome: "queued",
	}
	runSpec := model.RunSpec{RepoID: repository.ID, RefKind: model.RefBranch, Ref: event.Ref, SHA: sha, TestedSHA: sha, Actor: event.Actor, Trigger: "branch"}
	type enqueueResult struct {
		value model.EnqueueRunResult
		err   error
	}
	enqueues := make(chan enqueueResult, 2)
	start = make(chan struct{})
	for _, client := range []*Store{first, second} {
		go func(client *Store) {
			<-start
			value, err := client.EnqueueReceiveEvent(ctx, event, runSpec)
			enqueues <- enqueueResult{value: value, err: err}
		}(client)
	}
	close(start)
	var runID string
	duplicates = 0
	for range 2 {
		result := <-enqueues
		if result.err != nil {
			t.Fatal(result.err)
		}
		if runID == "" {
			runID = result.value.ID
		} else if result.value.ID != runID {
			t.Fatalf("replay created run IDs %q and %q", runID, result.value.ID)
		}
		if result.value.Duplicate {
			duplicates++
		}
	}
	if duplicates != 1 {
		t.Fatalf("enqueue duplicate results = %d, want one", duplicates)
	}
	var eventCount, runCount, auditCount int
	if err := first.db.QueryRowContext(ctx, `SELECT count(*) FROM receive_events WHERE id IN ('receive-delete', 'receive-enqueue')`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := first.db.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE id = ?`, runID).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if err := first.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_actions WHERE resource_type = 'receive'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 2 || runCount != 1 || auditCount != 2 {
		t.Fatalf("durable counts events=%d runs=%d audits=%d", eventCount, runCount, auditCount)
	}

	concurrent := make(chan error, 2)
	start = make(chan struct{})
	for index, client := range []*Store{first, second} {
		index, client := index, client
		go func() {
			<-start
			candidate := strings.Repeat(string(rune('b'+index)), 40)
			_, err := client.EnqueueRun(ctx, testRunSpec(repository.ID, "feature/concurrent", candidate))
			concurrent <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-concurrent; err != nil {
			t.Fatalf("concurrent enqueue: %v", err)
		}
	}
}

func TestIssueLockExpiryOwnershipAndRenewal(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	issue, err := s.CreateManualIssue(context.Background(), "agent@host", model.ManualIssueSpec{Title: "locked", Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	lock, err := s.AcquireIssueLock(ctx, issue.ID, "SHA256:alice")
	if err != nil {
		t.Fatal(err)
	}
	if lock.ExpiresAt.Sub(lock.AcquiredAt) != IssueLockTTL {
		t.Fatalf("lock TTL = %s, want %s", lock.ExpiresAt.Sub(lock.AcquiredAt), IssueLockTTL)
	}
	if _, err := s.AcquireIssueLock(ctx, issue.ID, "SHA256:bob"); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second owner error = %v, want ErrLockHeld", err)
	}
	now = now.Add(2 * time.Minute)
	renewed, err := s.RenewIssueLock(ctx, issue.ID, "SHA256:alice")
	if err != nil || renewed.ExpiresAt.Sub(now) != IssueLockTTL {
		t.Fatalf("renewed lock = %#v, %v", renewed, err)
	}
	now = now.Add(IssueLockTTL + time.Second)
	if _, err := s.RenewIssueLock(ctx, issue.ID, "SHA256:alice"); !errors.Is(err, ErrLockNotOwned) {
		t.Fatalf("expired renewal error = %v, want ErrLockNotOwned", err)
	}
	if _, err := s.AcquireIssueLock(ctx, issue.ID, "SHA256:bob"); err != nil {
		t.Fatalf("acquire after expiry: %v", err)
	}
	if err := s.ReleaseIssueLock(ctx, issue.ID, "SHA256:bob"); err != nil {
		t.Fatal(err)
	}
	var actions int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_actions WHERE resource_type = 'issue' AND action LIKE 'issue.lock.%'`).Scan(&actions); err != nil || actions != 4 {
		t.Fatalf("issue lock audit actions = %d, %v", actions, err)
	}
}

func TestIssueMutationsHonorActiveLockOwner(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	manual, err := s.CreateManualIssue(ctx, "SHA256:alice", model.ManualIssueSpec{Title: "manual", Body: "original"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireIssueLock(ctx, manual.ID, "SHA256:alice"); err != nil {
		t.Fatal(err)
	}
	changed := "changed"
	if _, err := s.UpdateManualIssue(ctx, "SHA256:bob", manual.ID, model.IssuePatch{Body: &changed}); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("non-owner update error = %v, want ErrLockHeld", err)
	}
	if err := s.DeleteManualIssue(ctx, "SHA256:bob", manual.ID); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("non-owner delete error = %v, want ErrLockHeld", err)
	}
	stored, err := s.Issue(ctx, manual.ID)
	if err != nil || stored.Body != "original" {
		t.Fatalf("manual issue after rejected mutations = %#v, %v", stored, err)
	}

	now = now.Add(2 * time.Minute)
	if _, err := s.UpdateManualIssue(ctx, "SHA256:alice", manual.ID, model.IssuePatch{Body: &changed}); err != nil {
		t.Fatalf("owner update: %v", err)
	}
	now = now.Add(IssueLockTTL - time.Minute)
	if _, err := s.RenewIssueLock(ctx, manual.ID, "SHA256:alice"); err != nil {
		t.Fatalf("mutation did not renew owner lock atomically: %v", err)
	}

	deletable, err := s.CreateManualIssue(ctx, "SHA256:alice", model.ManualIssueSpec{Title: "delete me"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireIssueLock(ctx, deletable.ID, "SHA256:alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteManualIssue(ctx, "SHA256:alice", deletable.ID); err != nil {
		t.Fatalf("owner delete: %v", err)
	}

	ciIssue, err := s.UpsertCIIssue(ctx, "SHA256:alice", createRepo(t, s).ID, "feature/locked", "red", "failed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseCIIssue(ctx, "SHA256:bob", ciIssue.ID, "wrong owner"); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("non-owner CI close error = %v, want ErrLockHeld", err)
	}
	if _, err := s.CloseCIIssue(ctx, "SHA256:alice", ciIssue.ID, "resolved"); err != nil {
		t.Fatalf("owner CI close: %v", err)
	}

	expired, err := s.CreateManualIssue(ctx, "SHA256:alice", model.ManualIssueSpec{Title: "expired", Body: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireIssueLock(ctx, expired.ID, "SHA256:alice"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(IssueLockTTL + time.Second)
	if _, err := s.UpdateManualIssue(ctx, "SHA256:bob", expired.ID, model.IssuePatch{Body: &changed}); err != nil {
		t.Fatalf("mutation after lock expiry: %v", err)
	}
}

func TestUplinksAndAuditActions(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	credential, err := s.CreateTokenCredential(context.Background(), model.TokenCredentialSpec{
		Name: "operator", Digest: make([]byte, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := s.UpsertUplink(context.Background(), model.UplinkSpec{
		Fingerprint: "SHA256:key", Identity: "operator@example.com",
		TokenCredentialID: credential.ID, AuthActor: "token:" + credential.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	credential, err = s.ActivateTokenCredential(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if uplink.Identity != "operator@example.com" || uplink.TokenCredentialID != credential.ID || uplink.AuthActor == "" {
		t.Fatalf("uplink = %#v", uplink)
	}
	if _, err := s.UpsertUplink(context.Background(), model.UplinkSpec{
		Fingerprint: "SHA256:other", Identity: "other@example.com", TokenCredentialID: credential.ID, AuthActor: "bootstrap",
	}); err == nil {
		t.Fatal("credential bound to a second uplink")
	}
	action, err := s.AppendAuditAction(context.Background(), model.AuditActionSpec{
		Actor: uplink.Identity, Action: "issue.lock", ResourceType: "issue", ResourceID: "1", Details: `{"owner":"SHA256:key"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if action.CreatedAt.IsZero() {
		t.Fatal("audit action timestamp is empty")
	}
	if _, err := s.db.ExecContext(context.Background(), `DELETE FROM audit_actions WHERE id = ?`, action.ID); err == nil {
		t.Fatal("audit action delete unexpectedly succeeded")
	}
}

func TestGenericIssueUpdatePreservesCIProjectionOwnership(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	repository := createRepo(t, s)
	issue, err := s.UpsertCIIssue(ctx, "agent@host", repository.ID, "feature/ci-update", "red", "original")
	if err != nil {
		t.Fatal(err)
	}
	newTitle, newBody := "still red", "updated diagnosis"
	updated, err := s.UpdateIssue(ctx, "agent@host", issue.ID, model.IssuePatch{Title: &newTitle, Body: &newBody})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Kind != model.IssueCI || updated.RepoID != repository.ID || updated.Branch != "feature/ci-update" ||
		updated.Title != newTitle || updated.Body != newBody {
		t.Fatalf("updated CI issue = %#v", updated)
	}
}

func TestClaimEmptyQueue(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	if _, err := s.ClaimNextRun(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("empty claim error = %v, want sql.ErrNoRows", err)
	}
}

func TestCreateGenesisRejectsQuestionMarkInPath(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite?mode=rw")
	_, err := CreateGenesis(ctx, path, Options{})
	if err == nil {
		t.Fatal("CreateGenesis accepted a path containing '?'")
	}
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "?") {
		t.Fatalf("unexpected error: %v", err)
	}
	// The placeholder file must not exist.
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("placeholder file was left behind: %v", statErr)
	}
}

func TestCreateGenesisRemovesPlaceholderOnInitFailure(t *testing.T) {
	ctx := context.Background()
	// Use a path in a directory that exists for file creation but will
	// fail during open() because we cancel the context.
	dir := t.TempDir()
	path := filepath.Join(dir, "genesis-fail.sqlite")
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel() // already canceled
	_, err := CreateGenesis(canceledCtx, path, Options{})
	if err == nil {
		t.Fatal("CreateGenesis succeeded with canceled context")
	}
	// The placeholder must be cleaned up.
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("placeholder file was left behind after initialization failure")
	}
}

func TestResolveRunRefsPrefixDisambiguation(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	const refName = "v1.0"
	branchSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tagSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// Create a branch run and a tag run with the same ref name.
	branchRun, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, refName, branchSHA))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	tagSpec := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefTag, Ref: refName, SHA: tagSHA,
		Actor: "SHA256:test", Trigger: "tag",
	}
	tagRun, err := s.EnqueueRun(ctx, tagSpec)
	if err != nil {
		t.Fatal(err)
	}

	// Bare name with both kinds present is ambiguous.
	if _, err := s.ResolveRun(ctx, repo.ID, refName); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("bare ambiguous name error = %v, want ErrAmbiguous", err)
	}

	// refs/heads/<name> resolves to the branch run.
	resolved, err := s.ResolveRun(ctx, repo.ID, "refs/heads/"+refName)
	if err != nil {
		t.Fatalf("refs/heads/ resolve: %v", err)
	}
	if resolved.ID != branchRun.ID {
		t.Fatalf("refs/heads/ resolved run = %s, want branch run %s", resolved.ID, branchRun.ID)
	}

	// refs/tags/<name> resolves to the tag run.
	resolved, err = s.ResolveRun(ctx, repo.ID, "refs/tags/"+refName)
	if err != nil {
		t.Fatalf("refs/tags/ resolve: %v", err)
	}
	if resolved.ID != tagRun.ID {
		t.Fatalf("refs/tags/ resolved run = %s, want tag run %s", resolved.ID, tagRun.ID)
	}

	// refs/heads/<missing> returns not-found.
	if _, err := s.ResolveRun(ctx, repo.ID, "refs/heads/nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refs/heads/ missing error = %v, want ErrNotFound", err)
	}

	// refs/heads/ with empty remainder returns invalid.
	if _, err := s.ResolveRun(ctx, repo.ID, "refs/heads/"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("refs/heads/ empty error = %v, want ErrInvalid", err)
	}

	// refs/tags/ with empty remainder returns invalid.
	if _, err := s.ResolveRun(ctx, repo.ID, "refs/tags/"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("refs/tags/ empty error = %v, want ErrInvalid", err)
	}
}

func TestListLatestRunsPerRepoReturnsQuietReposOutsideGlobalWindow(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	upstream, err := s.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "forge", Kind: "forgejo", BaseURL: "https://codeberg.org",
	})
	if err != nil {
		t.Fatal(err)
	}
	repoA, err := s.CreateRepository(ctx, model.RepositorySpec{
		Name: "active-repo", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	repoB, err := s.CreateRepository(ctx, model.RepositorySpec{
		Name: "quiet-repo", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Seed 2 runs for repo B first (these will be oldest).
	for i := 0; i < 2; i++ {
		sha := fmt.Sprintf("b%039d", i)
		now = now.Add(time.Second)
		if _, err := s.EnqueueRun(ctx, testRunSpec(repoB.ID, "main", sha)); err != nil {
			t.Fatalf("enqueue quiet repo run %d: %v", i, err)
		}
	}

	// Seed 105 runs for repo A (pushes repo B out of any global top-100).
	for i := 0; i < 105; i++ {
		sha := fmt.Sprintf("a%039d", i)
		now = now.Add(time.Second)
		if _, err := s.EnqueueRun(ctx, testRunSpec(repoA.ID, "main", sha)); err != nil {
			t.Fatalf("enqueue active repo run %d: %v", i, err)
		}
	}

	// Verify the global ListRecentRuns at limit 100 misses repo B entirely.
	globalRuns, err := s.ListRecentRuns(ctx, model.RunListFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	hasQuietRepo := false
	for _, run := range globalRuns {
		if run.RepoID == repoB.ID {
			hasQuietRepo = true
			break
		}
	}
	if hasQuietRepo {
		t.Fatal("precondition: global top-100 should not contain quiet repo runs")
	}

	// ListLatestRunsPerRepo must return runs for both repos.
	perRepoRuns, err := s.ListLatestRunsPerRepo(ctx, 12)
	if err != nil {
		t.Fatal(err)
	}
	repoIDs := make(map[int64]int)
	for _, run := range perRepoRuns {
		repoIDs[run.RepoID]++
	}
	if repoIDs[repoA.ID] == 0 {
		t.Fatalf("per-repo runs missing active repo; got repos %v", repoIDs)
	}
	if repoIDs[repoB.ID] == 0 {
		t.Fatalf("per-repo runs missing quiet repo; got repos %v", repoIDs)
	}
	if repoIDs[repoB.ID] != 2 {
		t.Fatalf("quiet repo should have 2 runs, got %d", repoIDs[repoB.ID])
	}
	if repoIDs[repoA.ID] > 12 {
		t.Fatalf("active repo should have at most 12 runs, got %d", repoIDs[repoA.ID])
	}

	// Verify ordering: runs are ordered by queue_sequence DESC.
	for i := 1; i < len(perRepoRuns); i++ {
		if perRepoRuns[i].QueueSequence > perRepoRuns[i-1].QueueSequence {
			t.Fatalf("per-repo runs not ordered by queue_sequence DESC at index %d", i)
		}
	}
}
