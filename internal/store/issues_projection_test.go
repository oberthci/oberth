package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

const (
	projectionSourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	projectionBaseSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	projectionResultSHA = "cccccccccccccccccccccccccccccccccccccccc"
)

func projectionRepository(t *testing.T, database *Store) model.Repository {
	t.Helper()
	ctx := context.Background()
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := database.CreateRepository(ctx, model.RepositorySpec{
		Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func projectionRun(t *testing.T, database *Store, repoID int64, branch, sha string) model.Run {
	t.Helper()
	return projectionRunWithTrigger(t, database, repoID, branch, sha, "branch")
}

func projectionRunWithTrigger(t *testing.T, database *Store, repoID int64, branch, sha, trigger string) model.Run {
	t.Helper()
	ctx := context.Background()
	enqueued, err := database.EnqueueRun(ctx, model.RunSpec{
		RepoID: repoID, RefKind: model.RefBranch, Ref: branch, SHA: sha,
		Actor: "agent@host", Trigger: trigger, TestedSHA: sha,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := database.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != enqueued.ID {
		t.Fatalf("claimed run %s, want %s", claimed.ID, enqueued.ID)
	}
	return claimed
}

func TestInfrastructureRunRetriesWithoutProjectingCIRedIssue(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	repository := projectionRepository(t, database)
	ctx := context.Background()
	branch := repository.DefaultBranch
	run := projectionRun(t, database, repository.ID, branch, projectionSourceSHA)
	result := model.RunResult{Status: model.RunFailed, Phase: "infrastructure", Error: `Evicted: Usage of EmptyDir volume "var-run-argo" exceeds the limit "128Mi".`}
	if _, err := database.FinishRun(ctx, run.ID, result); err != nil {
		t.Fatal(err)
	}
	if _, err := database.OpenCIIssue(ctx, repository.ID, branch); !errors.Is(err, ErrNotFound) {
		t.Fatalf("infrastructure failure opened CI-red issue: %v", err)
	}
	retry, err := database.RetryRun(ctx, run.ID, "reviewer@host")
	if err != nil || retry.SHA != run.SHA {
		t.Fatalf("infrastructure retry = %#v, %v", retry, err)
	}
	// A real test failure still opens an issue. A later infrastructure failure
	// neither updates its evidence nor resolves it.
	if _, err := database.FinishRun(ctx, retry.ID, model.RunResult{Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit"}); err != nil {
		t.Fatal(err)
	}
	issue, err := database.OpenCIIssue(ctx, repository.ID, branch)
	if err != nil {
		t.Fatal(err)
	}
	next := projectionRun(t, database, repository.ID, branch, projectionResultSHA)
	if _, err := database.FinishRun(ctx, next.ID, result); err != nil {
		t.Fatal(err)
	}
	after, err := database.OpenCIIssue(ctx, repository.ID, branch)
	if err != nil || after.ID != issue.ID || after.Body != issue.Body || after.Occurrences != issue.Occurrences {
		t.Fatalf("infrastructure failure changed test issue: %#v, %v", after, err)
	}
}

func TestInfrastructurePromotionRunDoesNotProjectCIRedIssue(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	repository := projectionRepository(t, database)
	ctx := context.Background()
	spec := retryPromotionSpec(repository.ID, "")
	promotion, err := database.AppendPromotion(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	seedLegacyPromotionIssueWork(t, database, promotion)
	if _, err := database.PlanPromotion(ctx, promotion.ID, retryStoreBase, retryStoreMerged); err != nil {
		t.Fatal(err)
	}
	run, _, err := database.EnqueueAdmittedPromotionRun(ctx, model.RunSpec{
		RepoID: repository.ID, RefKind: model.RefBranch, Ref: "promotion/main/" + retryStoreMerged[:12],
		SHA: retryStoreMerged, Actor: spec.Actor, Trigger: "promotion", TestedSHA: retryStoreMerged, BaseSHA: retryStoreBase,
	}, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunFailed, Phase: "infrastructure", Error: "main: OOMKilled"}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.OpenCIIssue(ctx, repository.ID, spec.SourceBranch); !errors.Is(err, ErrNotFound) {
		t.Fatalf("infrastructure promotion opened CI-red issue: %v", err)
	}
	finished, err := database.Promotion(ctx, promotion.ID)
	if err != nil || finished.Status != model.PromotionFailed || finished.Error != "main: OOMKilled" {
		t.Fatalf("promotion lost failure: %#v, %v", finished, err)
	}
}

func TestRunAndIssueProjectionCommitAtomically(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	repository := projectionRepository(t, database)
	run := projectionRun(t, database, repository.ID, "main", projectionSourceSHA)
	ctx := context.Background()

	if _, err := database.db.ExecContext(ctx, `
CREATE TRIGGER inject_ci_issue_failure
BEFORE INSERT ON issues WHEN NEW.kind = 'ci'
BEGIN SELECT RAISE(ABORT, 'injected issue projection failure'); END;`); err != nil {
		t.Fatal(err)
	}
	_, err := database.FinishRun(ctx, run.ID, model.RunResult{
		Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit", Error: "red",
	})
	if err == nil || !strings.Contains(err.Error(), "injected issue projection failure") {
		t.Fatalf("FinishRun error = %v, want injected projection failure", err)
	}
	stillRunning, err := database.Run(ctx, run.ID)
	if err != nil || stillRunning.Status != model.RunRunning {
		t.Fatalf("run after projection rollback = %#v, %v", stillRunning, err)
	}
	if _, err := database.OpenCIIssue(ctx, repository.ID, "main"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("issue survived rolled-back terminal outcome: %v", err)
	}

	if _, err := database.db.ExecContext(ctx, `DROP TRIGGER inject_ci_issue_failure`); err != nil {
		t.Fatal(err)
	}
	finished, err := database.FinishRun(ctx, run.ID, model.RunResult{
		Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit", Error: "red",
	})
	if err != nil || finished.Status != model.RunFailed {
		t.Fatalf("finished run = %#v, %v", finished, err)
	}
	issue, err := database.OpenCIIssue(ctx, repository.ID, "main")
	if err != nil || issue.State != model.IssueOpen || !strings.Contains(issue.Body, run.ID) {
		t.Fatalf("projected issue = %#v, %v", issue, err)
	}
}

func TestRunIssueProjectionIncludesTransientFailureTailAndBurnLogHint(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	repository := projectionRepository(t, database)
	ctx := context.Background()
	branch := "main"

	first := projectionRun(t, database, repository.ID, branch, projectionSourceSHA)
	firstTail := "--- FAIL: TestResolve_TimeoutEnforced\nFAIL example/internal/registry"
	finished, err := database.FinishRun(ctx, first.ID, model.RunResult{
		Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit",
		Error: "step test/unit failed with exit code 1", FailureTail: firstTail,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(finished.Error, firstTail) {
		t.Fatalf("transient failure tail leaked into run row: %#v", finished)
	}
	issue, err := database.OpenCIIssue(ctx, repository.ID, branch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(issue.Body, firstTail) ||
		!strings.Contains(issue.Body, "full step log: run_logs "+first.ID+" test unit") {
		t.Fatalf("first projected issue lacks tail or burn-log hint: %#v", issue)
	}

	second := projectionRun(t, database, repository.ID, branch, projectionResultSHA)
	secondTail := "--- FAIL: TestResolve_NewFailure\nFAIL example/internal/newfailure"
	if _, err := database.FinishRun(ctx, second.ID, model.RunResult{
		Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit",
		Error: "step test/unit failed with exit code 1", FailureTail: secondTail,
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := database.OpenCIIssue(ctx, repository.ID, branch)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != issue.ID || updated.Occurrences != 2 || strings.Contains(updated.Body, firstTail) ||
		!strings.Contains(updated.Body, secondTail) ||
		!strings.Contains(updated.Body, "full step log: run_logs "+second.ID+" test unit") {
		t.Fatalf("updated projected issue does not contain only the new tail: %#v", updated)
	}
}

func TestCIIssueProjectionTakesNoLock(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	repository := projectionRepository(t, database)
	ctx := context.Background()
	branch := "main"

	first := projectionRun(t, database, repository.ID, branch, projectionSourceSHA)
	if _, err := database.FinishRun(ctx, first.ID, model.RunResult{Status: model.RunFailed, Phase: "test", Error: "first red"}); err != nil {
		t.Fatal(err)
	}
	issue, err := database.OpenCIIssue(ctx, repository.ID, branch)
	if err != nil {
		t.Fatal(err)
	}
	var locks int
	if err := database.db.QueryRowContext(ctx, `SELECT count(*) FROM issue_locks WHERE issue_id = ?`, issue.ID).Scan(&locks); err != nil || locks != 0 {
		t.Fatalf("projection locks = %d, %v, want none", locks, err)
	}
	if _, err := database.AppendIssueBody(ctx, "monitor@host", issue.ID, "investigating"); err != nil {
		t.Fatalf("another actor cannot append to projected issue: %v", err)
	}
	held, err := database.AcquireIssueLock(ctx, issue.ID, "monitor@host")
	if err != nil {
		t.Fatal(err)
	}
	lockAcquireAudits := func() int {
		t.Helper()
		var count int
		if err := database.db.QueryRowContext(ctx, `
SELECT count(*) FROM audit_actions
WHERE action = 'issue.lock.acquire' AND resource_type = 'issue' AND CAST(resource_id AS INTEGER) = ?`, issue.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	auditsBeforeProjection := lockAcquireAudits()

	second := projectionRun(t, database, repository.ID, branch, projectionResultSHA)
	if _, err := database.FinishRun(ctx, second.ID, model.RunResult{Status: model.RunFailed, Phase: "test", Error: "second red"}); err != nil {
		t.Fatal(err)
	}
	projected, err := database.OpenCIIssue(ctx, repository.ID, branch)
	if err != nil || projected.CIWorkID != second.ID {
		t.Fatalf("projected issue after second failure = %#v, %v", projected, err)
	}
	var owner string
	var acquiredAt, expiresAt int64
	if err := database.db.QueryRowContext(ctx, `
SELECT owner, acquired_at, expires_at FROM issue_locks WHERE issue_id = ?`, issue.ID).Scan(&owner, &acquiredAt, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if owner != held.Owner || acquiredAt != unixNano(held.AcquiredAt) || expiresAt != unixNano(held.ExpiresAt) {
		t.Fatalf("lock after projection = (%q, %d, %d), want unchanged %#v", owner, acquiredAt, expiresAt, held)
	}
	if audits := lockAcquireAudits(); audits != auditsBeforeProjection {
		t.Fatalf("lock-acquire audits after projection = %d, want %d", audits, auditsBeforeProjection)
	}

	now = now.Add(IssueLockTTL + time.Second)
	third := projectionRun(t, database, repository.ID, branch, "dddddddddddddddddddddddddddddddddddddddd")
	if _, err := database.FinishRun(ctx, third.ID, model.RunResult{Status: model.RunFailed, Phase: "test", Error: "third red"}); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, `SELECT owner FROM issue_locks WHERE issue_id = ?`, issue.ID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != held.Owner {
		t.Fatalf("projection replaced expired lock owner %q, want %q", owner, held.Owner)
	}
	if audits := lockAcquireAudits(); audits != auditsBeforeProjection {
		t.Fatalf("projection acquired an expired lock: audits = %d, want %d", audits, auditsBeforeProjection)
	}
	green := projectionRun(t, database, repository.ID, branch, projectionBaseSHA)
	if _, err := database.FinishRun(ctx, green.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, `SELECT owner FROM issue_locks WHERE issue_id = ?`, issue.ID).Scan(&owner); err != nil || owner != held.Owner {
		t.Fatalf("green projection changed existing lock: owner = %q, error = %v", owner, err)
	}
}

func TestPublicationAndIssueProjectionCommitAtomically(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	repository := projectionRepository(t, database)
	ctx := context.Background()

	red := projectionRun(t, database, repository.ID, "main", projectionSourceSHA)
	if _, err := database.FinishRun(ctx, red.ID, model.RunResult{Status: model.RunFailed, Phase: "test", Error: "red"}); err != nil {
		t.Fatal(err)
	}
	green := projectionRun(t, database, repository.ID, "main", projectionResultSHA)
	publication, err := database.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repository.ID, RunID: green.ID, RefKind: model.RefBranch,
		Ref: green.Ref, PreviousSHA: projectionSourceSHA, ResultSHA: projectionResultSHA,
		Actor: green.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `
CREATE TRIGGER inject_ci_issue_close_failure
BEFORE UPDATE ON issues WHEN OLD.kind = 'ci' AND NEW.state = 'closed'
BEGIN SELECT RAISE(ABORT, 'injected issue close failure'); END;`); err != nil {
		t.Fatal(err)
	}
	_, err = database.FinalizePublication(ctx, publication.ID, model.PublicationDelivered, "")
	if err == nil || !strings.Contains(err.Error(), "injected issue close failure") {
		t.Fatalf("FinalizePublication error = %v, want injected projection failure", err)
	}
	pending, err := database.Publication(ctx, publication.ID)
	if err != nil || pending.Status != model.PublicationPending {
		t.Fatalf("publication after rollback = %#v, %v", pending, err)
	}
	stillRunning, err := database.Run(ctx, green.ID)
	if err != nil || stillRunning.Status != model.RunRunning || stillRunning.Phase != "publishing" {
		t.Fatalf("publication run after rollback = %#v, %v", stillRunning, err)
	}
	if _, err := database.OpenCIIssue(ctx, repository.ID, green.Ref); err != nil {
		t.Fatalf("red issue disappeared during rolled-back green finalization: %v", err)
	}

	if _, err := database.db.ExecContext(ctx, `DROP TRIGGER inject_ci_issue_close_failure`); err != nil {
		t.Fatal(err)
	}
	finalization, err := database.FinalizePublication(ctx, publication.ID, model.PublicationDelivered, "")
	if err != nil || finalization.Run.Status != model.RunPassed {
		t.Fatalf("replayed publication finalization = %#v, %v", finalization, err)
	}
	if _, err := database.OpenCIIssue(ctx, repository.ID, green.Ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("green replay did not close issue: %v", err)
	}
}

func TestLegacyPromotionAndIssueProjectionCommitAtomically(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	repository := projectionRepository(t, database)
	ctx := context.Background()
	promotion, err := database.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repository.ID, SourceBranch: "feature/promotion-atomic", SourceSHA: projectionSourceSHA,
		TargetRef: "main", PreviousSHA: projectionBaseSHA, ResultSHA: projectionResultSHA,
		Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedLegacyPromotionIssueWork(t, database, promotion)
	if _, err := database.db.ExecContext(ctx, `
CREATE TRIGGER inject_promotion_issue_failure
BEFORE INSERT ON issues WHEN NEW.kind = 'ci'
BEGIN SELECT RAISE(ABORT, 'injected promotion issue failure'); END;`); err != nil {
		t.Fatal(err)
	}
	_, err = database.FinishPromotion(ctx, promotion.ID, model.PromotionFailed, "", "red")
	if err == nil || !strings.Contains(err.Error(), "injected promotion issue failure") {
		t.Fatalf("FinishPromotion error = %v, want injected projection failure", err)
	}
	pending, err := database.Promotion(ctx, promotion.ID)
	if err != nil || pending.Status != model.PromotionPending {
		t.Fatalf("promotion after rollback = %#v, %v", pending, err)
	}
}

func TestLegacyPromotionIssueProjectionIsOrderedAndProofAware(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	repository := projectionRepository(t, database)
	ctx := context.Background()
	branch := "feature/proof"
	spec := model.PromotionSpec{
		RepoID: repository.ID, SourceBranch: branch, SourceSHA: projectionSourceSHA,
		TargetRef: "main", PreviousSHA: projectionBaseSHA, ResultSHA: projectionResultSHA,
		Actor: "agent@host",
	}

	older, err := database.AppendPromotion(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := database.AppendPromotion(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	seedLegacyPromotionIssueWork(t, database, older)
	seedLegacyPromotionIssueWork(t, database, newer)
	if _, err := database.FinishPromotion(ctx, newer.ID, model.PromotionFailed, "", "newer promotion failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.FinishPromotion(ctx, older.ID, model.PromotionFailed, "", "delayed older promotion failed"); err != nil {
		t.Fatal(err)
	}
	issue, err := database.OpenCIIssue(ctx, repository.ID, branch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(issue.Body, newer.ID) || strings.Contains(issue.Body, older.ID) {
		t.Fatalf("delayed result overwrote newer projection: %#v", issue)
	}

	// LOGIC-8 (#447): a green branch run supersedes the stale promotion
	// failure and closes the CI issue. The branch code is healthy; the
	// promotion failure is a historical artifact.
	green := projectionRunWithTrigger(t, database, repository.ID, branch, projectionSourceSHA, "schedule")
	if _, err := database.FinishRun(ctx, green.ID, model.RunResult{Status: model.RunPassed, Phase: "passed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.OpenCIIssue(ctx, repository.ID, branch); !errors.Is(err, ErrNotFound) {
		t.Fatalf("green branch run should close stale promotion-failure issue: %v", err)
	}

	// A subsequent qualifying promotion with a delivered publication also
	// keeps the issue closed (no regression from the LOGIC-8 path).
	qualifying, err := database.AppendPromotion(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	seedLegacyPromotionIssueWork(t, database, qualifying)
	publication, err := database.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repository.ID, PromotionID: qualifying.ID, RefKind: model.RefBranch,
		Ref: qualifying.TargetRef, PreviousSHA: qualifying.PreviousSHA, ResultSHA: qualifying.ResultSHA,
		Actor: qualifying.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.FinalizePublication(ctx, publication.ID, model.PublicationDelivered, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := database.OpenCIIssue(ctx, repository.ID, branch); !errors.Is(err, ErrNotFound) {
		t.Fatalf("qualifying promotion did not keep issue closed: %v", err)
	}
}

func TestRestartInterruptedPromotionDoesNotProjectIssue(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := OpenAdminClient(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	repository := projectionRepository(t, database)
	promotion, err := database.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repository.ID, SourceBranch: "feature/restart", SourceSHA: projectionSourceSHA,
		TargetRef: "main", PreviousSHA: projectionBaseSHA, ResultSHA: projectionResultSHA,
		Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedLegacyPromotionIssueWork(t, database, promotion)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	recovered, err := database.Promotion(ctx, promotion.ID)
	if err != nil || recovered.Status != model.PromotionInterrupted {
		t.Fatalf("recovered promotion = %#v, %v", recovered, err)
	}
	issue, err := database.OpenCIIssue(ctx, repository.ID, "feature/restart")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("interrupted promotion created issue = %#v, %v", issue, err)
	}
}

// Recreate admission records retained by databases opened before the policy
// change, without enabling new promotion issue registration in production.
func seedLegacyPromotionIssueWork(t *testing.T, database *Store, promotion model.Promotion) {
	t.Helper()
	if _, err := database.db.ExecContext(context.Background(), `
INSERT INTO ci_issue_work(repo_id, branch, origin, promotion_id, actor, created_at)
VALUES(?, ?, 'promotion', ?, ?, ?)
ON CONFLICT(promotion_id) WHERE promotion_id IS NOT NULL DO NOTHING`, promotion.RepoID, promotion.SourceBranch, promotion.ID, promotion.Actor, unixNano(database.now())); err != nil {
		t.Fatal(err)
	}
}

func TestRunIssueProjectionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, defaultBranch, ref, trigger string
		refKind                           model.RefKind
		wantIssue                         bool
	}{
		{"feature branch", "main", "feature/red", "branch", model.RefBranch, false},
		{"default branch", "main", "main", "branch", model.RefBranch, true},
		{"custom default branch", "trunk", "trunk", "branch", model.RefBranch, true},
		{"main is not the default", "trunk", "main", "branch", model.RefBranch, false},
		{"scheduled feature branch", "main", "feature/nightly", "schedule", model.RefBranch, true},
		{"promotion", "main", "main", "promotion", model.RefBranch, false},
		{"plan", "main", "main", "plan", model.RefBranch, false},
		{"apply", "main", "main", "apply", model.RefBranch, false},
		{"release remains deferred", "main", "v1.0.0", "tag", model.RefTag, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
			database := testStore(t, &now)
			repository := projectionRepository(t, database)
			ctx := context.Background()
			if _, err := database.SetRepositoryDefaultBranch(ctx, repository.ID, tc.defaultBranch); err != nil {
				t.Fatal(err)
			}
			run, err := database.EnqueueRun(ctx, model.RunSpec{
				RepoID: repository.ID, RefKind: tc.refKind, Ref: tc.ref, SHA: projectionSourceSHA,
				Actor: "agent@host", Trigger: tc.trigger, Release: tc.refKind == model.RefTag,
			})
			if err != nil {
				t.Fatal(err)
			}
			var work int
			if err := database.db.QueryRowContext(ctx, `SELECT count(*) FROM ci_issue_work WHERE run_id = ?`, run.ID).Scan(&work); err != nil {
				t.Fatal(err)
			}
			if (work == 1) != tc.wantIssue || work > 1 {
				t.Fatalf("registered work = %d, want issue = %t", work, tc.wantIssue)
			}
			if _, err := database.ClaimNextRun(ctx); err != nil {
				t.Fatal(err)
			}
			// Promotion-trigger runs require an associated promotion; their
			// registration exclusion is checked above and end to end below.
			if tc.trigger == "promotion" {
				return
			}
			if _, err := database.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunFailed, Phase: "test"}); err != nil {
				t.Fatal(err)
			}
			issue, err := database.OpenCIIssue(ctx, repository.ID, tc.ref)
			if tc.wantIssue {
				if err != nil || issue.CIWorkID != run.ID || issue.Occurrences != 1 {
					t.Fatalf("projected issue = %#v, %v", issue, err)
				}
			} else if !errors.Is(err, ErrNotFound) {
				t.Fatalf("excluded run created an issue: %#v, %v", issue, err)
			}
		})
	}
}

func TestPromotionDoesNotRegisterCIIssueWork(t *testing.T) {
	for _, withRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("with_run=%t", withRun), func(t *testing.T) {
			now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
			database := testStore(t, &now)
			repository := projectionRepository(t, database)
			ctx := context.Background()
			spec := model.PromotionSpec{
				RepoID: repository.ID, SourceBranch: "feature/red", SourceSHA: projectionSourceSHA,
				TargetRef: "main", PreviousSHA: projectionBaseSHA, ResultSHA: projectionResultSHA, Actor: "agent@host",
			}
			var promotion model.Promotion
			var err error
			if withRun {
				_, promotion, err = database.EnqueuePromotionRun(ctx, model.RunSpec{
					RepoID: repository.ID, RefKind: model.RefBranch, Ref: "promotion/main/test", SHA: projectionResultSHA,
					Actor: spec.Actor, Trigger: "promotion", TestedSHA: projectionResultSHA, BaseSHA: projectionBaseSHA,
				}, spec)
			} else {
				promotion, err = database.AppendPromotion(ctx, spec)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.FinishPromotion(ctx, promotion.ID, model.PromotionFailed, promotion.RunID, "red"); err != nil {
				t.Fatal(err)
			}
			var work int
			if err := database.db.QueryRowContext(ctx, `SELECT count(*) FROM ci_issue_work`).Scan(&work); err != nil || work != 0 {
				t.Fatalf("promotion issue work = %d, %v, want none", work, err)
			}
			if issue, err := database.OpenCIIssue(ctx, repository.ID, spec.SourceBranch); !errors.Is(err, ErrNotFound) {
				t.Fatalf("promotion created an issue: %#v, %v", issue, err)
			}
		})
	}
}
