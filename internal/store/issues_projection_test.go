package store

import (
	"context"
	"errors"
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
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/cloudtaser",
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
	ctx := context.Background()
	enqueued, err := database.EnqueueRun(ctx, model.RunSpec{
		RepoID: repoID, RefKind: model.RefBranch, Ref: branch, SHA: sha,
		Actor: "agent@host", Trigger: "branch", TestedSHA: sha,
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

func TestRunAndIssueProjectionCommitAtomically(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	repository := projectionRepository(t, database)
	run := projectionRun(t, database, repository.ID, "feature/atomic", projectionSourceSHA)
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
	if _, err := database.OpenCIIssue(ctx, repository.ID, "feature/atomic"); !errors.Is(err, ErrNotFound) {
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
	issue, err := database.OpenCIIssue(ctx, repository.ID, "feature/atomic")
	if err != nil || issue.State != model.IssueOpen || !strings.Contains(issue.Body, run.ID) {
		t.Fatalf("projected issue = %#v, %v", issue, err)
	}
}

func TestRunIssueProjectionIncludesTransientFailureTailAndBurnLogHint(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	repository := projectionRepository(t, database)
	ctx := context.Background()
	branch := "feature/failure-tail"

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
		!strings.Contains(issue.Body, "full step log: logs "+projectionSourceSHA+" unit") {
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
		!strings.Contains(updated.Body, "full step log: logs "+projectionResultSHA+" unit") {
		t.Fatalf("updated projected issue does not contain only the new tail: %#v", updated)
	}
}

func TestCIIssueProjectionDoesNotStealUnexpiredLock(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	repository := projectionRepository(t, database)
	ctx := context.Background()
	branch := "feature/lock-preservation"

	first := projectionRun(t, database, repository.ID, branch, projectionSourceSHA)
	if _, err := database.FinishRun(ctx, first.ID, model.RunResult{Status: model.RunFailed, Phase: "test", Error: "first red"}); err != nil {
		t.Fatal(err)
	}
	issue, err := database.OpenCIIssue(ctx, repository.ID, branch)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ReleaseIssueLock(ctx, issue.ID, first.Actor); err != nil {
		t.Fatal(err)
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
	auditsBeforeBlockedProjection := lockAcquireAudits()

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
		t.Fatalf("lock after blocked projection = (%q, %d, %d), want unchanged %#v", owner, acquiredAt, expiresAt, held)
	}
	if audits := lockAcquireAudits(); audits != auditsBeforeBlockedProjection {
		t.Fatalf("lock-acquire audits after blocked projection = %d, want %d", audits, auditsBeforeBlockedProjection)
	}

	now = now.Add(IssueLockTTL + time.Second)
	third := projectionRun(t, database, repository.ID, branch, "dddddddddddddddddddddddddddddddddddddddd")
	if _, err := database.FinishRun(ctx, third.ID, model.RunResult{Status: model.RunFailed, Phase: "test", Error: "third red"}); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, `SELECT owner FROM issue_locks WHERE issue_id = ?`, issue.ID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != third.Actor {
		t.Fatalf("lock owner after expired takeover = %q, want %q", owner, third.Actor)
	}
	if audits := lockAcquireAudits(); audits != auditsBeforeBlockedProjection+1 {
		t.Fatalf("lock-acquire audits after expired takeover = %d, want %d", audits, auditsBeforeBlockedProjection+1)
	}
}

func TestPublicationAndIssueProjectionCommitAtomically(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	repository := projectionRepository(t, database)
	ctx := context.Background()

	red := projectionRun(t, database, repository.ID, "feature/publication", projectionSourceSHA)
	if _, err := database.FinishRun(ctx, red.ID, model.RunResult{Status: model.RunFailed, Phase: "test", Error: "red"}); err != nil {
		t.Fatal(err)
	}
	green := projectionRun(t, database, repository.ID, "feature/publication", projectionResultSHA)
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

func TestPromotionAndIssueProjectionCommitAtomically(t *testing.T) {
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

func TestPromotionIssueProjectionIsOrderedAndProofAware(t *testing.T) {
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

	green := projectionRun(t, database, repository.ID, branch, projectionSourceSHA)
	if _, err := database.FinishRun(ctx, green.ID, model.RunResult{Status: model.RunPassed, Phase: "passed"}); err != nil {
		t.Fatal(err)
	}
	issue, err = database.OpenCIIssue(ctx, repository.ID, branch)
	if err != nil || !strings.Contains(issue.Body, newer.ID) {
		t.Fatalf("ordinary green resolved promotion proof: %#v, %v", issue, err)
	}

	qualifying, err := database.AppendPromotion(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("qualifying promotion did not resolve issue: %v", err)
	}
}

func TestRestartProjectsInterruptedPromotionAtomically(t *testing.T) {
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
	if err != nil || !strings.Contains(issue.Body, promotion.ID) {
		t.Fatalf("restart issue projection = %#v, %v", issue, err)
	}
}
