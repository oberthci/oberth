package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestAutoCloseCIIssueRecordsReasonAndResolvesOldProjection(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := projectionRepository(t, s)
	ctx := context.Background()
	run := projectionRunWithTrigger(t, s, repo.ID, "feature/red", projectionSourceSHA, "schedule")
	if _, err := s.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunFailed, Phase: "test", FailedStep: "unit"}); err != nil {
		t.Fatal(err)
	}
	issue, err := s.OpenCIIssue(ctx, repo.ID, "feature/red")
	if err != nil {
		t.Fatal(err)
	}
	const reason = "branch merged into main after green CI"
	closed, err := s.AutoCloseCIIssue(ctx, "oberth:ci-issue-reconciler", issue, reason)
	if err != nil || !closed {
		t.Fatalf("auto-close = %t, %v", closed, err)
	}
	got, err := s.Issue(ctx, issue.ID)
	if err != nil || got.State != model.IssueClosed || !strings.Contains(got.Body, reason) {
		t.Fatalf("closed issue = %#v, %v", got, err)
	}
	if _, err := s.OpenCIIssue(ctx, repo.ID, "feature/red"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open issue after close = %v", err)
	}
	var outcome string
	if err := s.db.QueryRowContext(ctx, `SELECT outcome FROM ci_issue_projections WHERE repo_id = ? AND branch = ? AND origin = 'branch'`, repo.ID, issue.Branch).Scan(&outcome); err != nil || outcome != "passed" {
		t.Fatalf("old projection outcome = %q, %v", outcome, err)
	}
	var actor, details string
	if err := s.db.QueryRowContext(ctx, `SELECT actor, details FROM audit_actions WHERE action = 'issue.ci.close' AND resource_id = ? ORDER BY id DESC LIMIT 1`, strconv.FormatInt(issue.ID, 10)).Scan(&actor, &details); err != nil || actor != "oberth:ci-issue-reconciler" || !strings.Contains(details, reason) {
		t.Fatalf("close history = actor %q, details %q, error %v", actor, details, err)
	}
	if again, err := s.AutoCloseCIIssue(ctx, "oberth:ci-issue-reconciler", issue, reason); err != nil || again {
		t.Fatalf("repeat close = %t, %v", again, err)
	}
}

func TestAutoCloseCIIssueGuardsDefaultBranchAndNewerFailure(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := projectionRepository(t, s)
	ctx := context.Background()
	mainIssue, err := s.UpsertCIIssue(ctx, "agent", repo.ID, "main", "red", "failure")
	if err != nil {
		t.Fatal(err)
	}
	if closed, err := s.AutoCloseCIIssue(ctx, "oberth:ci-issue-reconciler", mainIssue, "branch deleted upstream"); err != nil || closed {
		t.Fatalf("default-branch close = %t, %v", closed, err)
	}
	if _, err := s.OpenCIIssue(ctx, repo.ID, "main"); err != nil {
		t.Fatalf("default-branch issue was closed: %v", err)
	}
	first := projectionRunWithTrigger(t, s, repo.ID, "feature/red", projectionSourceSHA, "schedule")
	if _, err := s.FinishRun(ctx, first.ID, model.RunResult{Status: model.RunFailed, Phase: "test"}); err != nil {
		t.Fatal(err)
	}
	stale, err := s.OpenCIIssue(ctx, repo.ID, "feature/red")
	if err != nil {
		t.Fatal(err)
	}
	second := projectionRunWithTrigger(t, s, repo.ID, "feature/red", projectionResultSHA, "schedule")
	if _, err := s.FinishRun(ctx, second.ID, model.RunResult{Status: model.RunFailed, Phase: "test"}); err != nil {
		t.Fatal(err)
	}
	if closed, err := s.AutoCloseCIIssue(ctx, "oberth:ci-issue-reconciler", stale, "stale merge observation"); err != nil || closed {
		t.Fatalf("stale close = %t, %v", closed, err)
	}
	if _, err := s.OpenCIIssue(ctx, repo.ID, "feature/red"); err != nil {
		t.Fatalf("newer failure was closed: %v", err)
	}
}

func TestCIBranchDeletionEvidenceDoesNotTreatFirstRedRunAsDeletion(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := projectionRepository(t, s)
	ctx := context.Background()
	if known, err := s.CIBranchWasPublishedOrDeleted(ctx, repo.ID, "feature/red"); err != nil || known {
		t.Fatalf("unpublished branch known upstream = %t, %v", known, err)
	}
	if _, err := s.RecordReceiveEvent(ctx, model.ReceiveEventSpec{
		ID: "delete-feature-red", Actor: "agent", RepoID: repo.ID,
		RefKind: model.RefBranch, Ref: "feature/red", Outcome: "deleted",
	}); err != nil {
		t.Fatal(err)
	}
	if known, err := s.CIBranchWasPublishedOrDeleted(ctx, repo.ID, "feature/red"); err != nil || !known {
		t.Fatalf("explicit deletion known upstream = %t, %v", known, err)
	}
}

func TestCIBranchDeletionEvidenceIncludesDeliveredPublication(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := projectionRepository(t, s)
	ctx := context.Background()
	run := projectionRun(t, s, repo.ID, "feature/published", projectionSourceSHA)
	publication, err := s.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repo.ID, RunID: run.ID, RefKind: model.RefBranch, Ref: run.Ref,
		ResultSHA: run.SHA, Actor: run.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinalizePublication(ctx, publication.ID, model.PublicationDelivered, ""); err != nil {
		t.Fatal(err)
	}
	if known, err := s.CIBranchWasPublishedOrDeleted(ctx, repo.ID, run.Ref); err != nil || !known {
		t.Fatalf("published branch known upstream = %t, %v", known, err)
	}
}
