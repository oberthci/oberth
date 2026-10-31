package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRepositoryArchiveRetainsHistoryAndRefusesNewWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	db := testStore(t, &now)
	repo := createRepo(t, db)
	queued, err := db.EnqueueRun(ctx, testRunSpec(repo.ID, "feature", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetRepositoryArchived(ctx, "admin", repo.Name, true); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("archive queued = %v", err)
	}
	if _, err := db.db.Exec(`UPDATE runs SET status='passed',phase='passed',finished_at=? WHERE id=?`, unixNano(now), queued.ID); err != nil {
		t.Fatal(err)
	}
	archived, err := db.SetRepositoryArchived(ctx, "admin", repo.Name, true)
	if err != nil {
		t.Fatal(err)
	}
	if archived.ID != repo.ID || archived.ArchivedAt == nil || archived.ArchivedBy != "admin" {
		t.Fatalf("archived = %+v", archived)
	}
	if _, err := db.Run(ctx, queued.ID); err != nil {
		t.Fatalf("history lost: %v", err)
	}
	got, err := db.RepositoryByName(ctx, repo.Name)
	if err != nil || got.ArchivedAt == nil {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
	repos, err := db.ListRepositories(ctx)
	if err != nil || len(repos) != 1 || repos[0].ArchivedAt == nil {
		t.Fatalf("catalog = %+v, %v", repos, err)
	}
	for _, trigger := range []string{"push", "schedule", "manual", "retry"} {
		spec := testRunSpec(repo.ID, trigger, strings.Repeat("b", 40))
		spec.Trigger = trigger
		if _, err := db.EnqueueRun(ctx, spec); !errors.Is(err, ErrRepositoryArchived) {
			t.Fatalf("%s enqueue = %v", trigger, err)
		}
	}
	_, err = db.AppendPromotion(ctx, model.PromotionSpec{RepoID: repo.ID, SourceBranch: "feature", SourceSHA: strings.Repeat("a", 40), TargetRef: "main", PreviousSHA: strings.Repeat("b", 40), ResultSHA: strings.Repeat("c", 40), Actor: "admin"})
	if !errors.Is(err, ErrRepositoryArchived) {
		t.Fatalf("promotion = %v", err)
	}
	// Retrying archive is safe and emits no duplicate transition audit.
	if _, err := db.SetRepositoryArchived(ctx, "admin", repo.Name, true); err != nil {
		t.Fatal(err)
	}
	restored, err := db.SetRepositoryArchived(ctx, "admin", repo.Name, false)
	if err != nil || restored.ID != repo.ID || restored.ArchivedAt != nil || restored.ArchivedBy != "" {
		t.Fatalf("restored = %+v, %v", restored, err)
	}
	if _, err := db.EnqueueRun(ctx, testRunSpec(repo.ID, "new", strings.Repeat("b", 40))); err != nil {
		t.Fatalf("restored enqueue = %v", err)
	}
	var audit int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM audit_actions WHERE action IN ('repository.archive','repository.unarchive')`).Scan(&audit); err != nil || audit != 2 {
		t.Fatalf("audit = %d, %v", audit, err)
	}
}

func TestRepositoryArchiveRefusesPendingPromotionAndAuditFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()
	db := testStore(t, &now)
	repo := createRepo(t, db)
	promotion, err := db.AppendPromotion(ctx, model.PromotionSpec{RepoID: repo.ID, SourceBranch: "work", SourceSHA: strings.Repeat("a", 40), TargetRef: "main", PreviousSHA: strings.Repeat("b", 40), ResultSHA: strings.Repeat("c", 40), Actor: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetRepositoryArchived(ctx, "admin", repo.Name, true); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("pending promotion archive = %v", err)
	}
	if _, err := db.db.Exec(`UPDATE promotions SET status='failed' WHERE id=?`, promotion.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`CREATE TRIGGER archive_audit_failure BEFORE INSERT ON audit_actions WHEN NEW.action='repository.archive' BEGIN SELECT RAISE(ABORT,'test audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetRepositoryArchived(ctx, "admin", repo.Name, true); err == nil {
		t.Fatal("archive succeeded without audit")
	}
	got, err := db.Repository(ctx, repo.ID)
	if err != nil || got.ArchivedAt != nil {
		t.Fatalf("archive did not roll back: %+v, %v", got, err)
	}
}

func TestArchiveMetadataResolvesAllRepositorySelectors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()
	db := testStore(t, &now)
	upstream, err := db.CreateUpstream(ctx, model.UpstreamSpec{Name: "github", Kind: "ssh", BaseURL: "ssh://git@example.invalid/org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := db.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetRepositoryArchived(ctx, "admin", "github/org/repo", true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"repo", "org/repo", "github/org/repo"} {
		got, err := db.RepositoryByName(ctx, name)
		if err != nil || got.ID != repo.ID || got.ArchivedAt == nil || got.ArchivedBy != "admin" {
			t.Fatalf("%s = %+v, %v", name, got, err)
		}
	}
}

func TestArchivedRepositoryRefusesPublicationRetryWithoutReactivation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()
	db := testStore(t, &now)
	repo := createRepo(t, db)
	run, err := db.EnqueueRun(ctx, testRunSpec(repo.ID, "feature", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	publication, err := db.BeginPublication(ctx, model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, PreviousSHA: strings.Repeat("b", 40), ResultSHA: run.SHA, Actor: run.Actor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.FinalizePublication(ctx, publication.ID, model.PublicationFailed, "forge unavailable"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetRepositoryArchived(ctx, "admin", repo.Name, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RetryFailedPublication(ctx, run.ID, "", "admin"); !errors.Is(err, ErrRepositoryArchived) {
		t.Fatalf("publication retry = %v", err)
	}
	got, err := db.Run(ctx, run.ID)
	if err != nil || got.Status != model.RunFailed || got.Phase != "publishing" {
		t.Fatalf("reactivated archived run: %+v, %v", got, err)
	}
	pub, err := db.Publication(ctx, publication.ID)
	if err != nil || pub.Status != model.PublicationFailed {
		t.Fatalf("changed archived publication: %+v, %v", pub, err)
	}
	if _, err := db.RetryRun(ctx, model.RunRetryRequest{RunID: run.ID, Actor: "admin"}); !errors.Is(err, ErrRepositoryArchived) {
		t.Fatalf("run retry = %v", err)
	}
	authorized := false
	_, err = db.RerunRun(ctx, model.RunRerunRequest{RunID: run.ID, Actor: "admin", Authorize: func(model.Run, model.RunReplayEvidence) (string, error) { authorized = true; return "", nil }})
	if !errors.Is(err, ErrRepositoryArchived) || authorized {
		t.Fatalf("rerun = %v; contacted authorizer = %v", err, authorized)
	}
}

func TestRepositoryArchiveProjectionUsesBoundedAuditIndex(t *testing.T) {
	t.Parallel()
	now := time.Now()
	db := testStore(t, &now)
	repo := createRepo(t, db)
	rows, err := db.db.Query(`EXPLAIN QUERY PLAN SELECT `+repositoryColumns+` FROM repositories r WHERE r.id = ?`, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "SCAN audit_actions") {
			t.Fatalf("archive projection scans audit history: %s", detail)
		}
		indexed = indexed || strings.Contains(detail, "repository_archive_actions_idx")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("archive projection does not use its scoped audit index")
	}
}

func TestRepositoryArchiveMigrationPreservesV28HistoryAndLedger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v28.sqlite")
	original := time.Unix(100, 0)
	db, err := Open(ctx, path, Options{Now: func() time.Time { return original }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, db)
	run, err := db.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/history", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// v30 (#1128) stacks after v29: a real v28 database has neither.
	if _, err := raw.ExecContext(ctx, `DROP TABLE IF EXISTS step_resource_usage; DROP INDEX IF EXISTS runs_latest_ref_idx; DROP TABLE IF EXISTS run_priority_overrides; DROP TABLE IF EXISTS queue_priority_patterns; DELETE FROM schema_migrations WHERE version>=34; ALTER TABLE step_results DROP COLUMN memory_peak_bytes; DELETE FROM schema_migrations WHERE version=33; ALTER TABLE schedule_fires DROP COLUMN last_fired_at; DELETE FROM schema_migrations WHERE version=32; DROP TRIGGER run_schedule_entry_immutable; ALTER TABLE runs DROP COLUMN schedule_entry; DELETE FROM schema_migrations WHERE version=31; DROP TRIGGER run_timing_claim; DROP TRIGGER run_timing_grant; DROP TRIGGER run_timing_park; DROP TRIGGER run_timing_terminal; DROP TABLE step_timings; DROP TABLE run_admission_timings; DROP TABLE run_timings;
DROP INDEX repository_archive_actions_idx;
DELETE FROM schema_migrations WHERE version >= ?`, repositoryArchiveMigrationVersion); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded := original.Add(time.Hour)
	db, err = Open(ctx, path, Options{Now: func() time.Time { return upgraded }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	assertFreshSchemaLedger(t, db.db)
	for version := 1; version <= latestMigrationVersion; version++ {
		want := unixNano(original)
		if version >= repositoryArchiveMigrationVersion {
			want = unixNano(upgraded)
		}
		var appliedAt int64
		if err := db.db.QueryRowContext(ctx, `SELECT applied_at FROM schema_migrations WHERE version = ?`, version).Scan(&appliedAt); err != nil || appliedAt != want {
			t.Fatalf("migration %d applied_at=%d, want %d: %v", version, appliedAt, want, err)
		}
	}
	var indexes int
	if err := db.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name='repository_archive_actions_idx'`).Scan(&indexes); err != nil || indexes != 1 {
		t.Fatalf("archive index count=%d: %v", indexes, err)
	}
	got, err := db.Run(ctx, run.ID)
	if err != nil || got.QueueSequence != run.QueueSequence || got.AdmissionSequence != run.AdmissionSequence {
		t.Fatalf("migration changed run identity/order: %+v, %v", got, err)
	}
	gotRepo, err := db.Repository(ctx, repo.ID)
	if err != nil || gotRepo.Name != repo.Name || gotRepo.ArchivedAt != nil {
		t.Fatalf("migration changed repository: %+v, %v", gotRepo, err)
	}
	if _, err := db.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
}
