package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testdb"
)

func TestIssueWorkspaceStatusIsDurableLockedAndRevisioned(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	issue, err := s.CreateManualIssue(t.Context(), "writer", model.ManualIssueSpec{Title: "work", Body: "checkpoint"})
	if err != nil {
		t.Fatal(err)
	}
	var status string
	if err = s.db.QueryRow(`SELECT work_status FROM issues WHERE id=?`, issue.ID).Scan(&status); err != nil || status != "todo" {
		t.Fatalf("default status=%q: %v", status, err)
	}
	for _, want := range []string{"active", "waiting", "review", "todo"} {
		var patch model.IssuePatch
		if err = json.Unmarshal([]byte(`{"WorkStatus":"`+want+`"}`), &patch); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
		updated, err := s.UpdateIssue(t.Context(), "writer", issue.ID, patch)
		if err != nil {
			t.Fatal(err)
		}
		if updated.Revision != issue.Revision+1 || updated.State != model.IssueOpen || updated.Body != issue.Body || !updated.UpdatedAt.Equal(now) {
			t.Fatalf("status changed lifecycle/content: %+v", updated)
		}
		if err = s.db.QueryRow(`SELECT work_status FROM issues WHERE id=?`, issue.ID).Scan(&status); err != nil || status != want {
			t.Fatalf("status=%q: %v", status, err)
		}
		issue = updated
	}
	// Repeating a status is a checkpoint, not a new metadata revision.
	same := model.IssueTodo
	now = now.Add(time.Minute)
	checkpoint, err := s.UpdateIssue(t.Context(), "writer", issue.ID, model.IssuePatch{WorkStatus: &same})
	if err != nil || checkpoint.Revision != issue.Revision || !checkpoint.UpdatedAt.Equal(now) {
		t.Fatalf("unchanged status checkpoint=%+v %v", checkpoint, err)
	}
	issue = checkpoint
	staleRevision := issue.Revision - 1
	active := model.IssueActive
	if _, err = s.UpdateIssue(t.Context(), "writer", issue.ID, model.IssuePatch{WorkStatus: &active, ExpectedRevision: &staleRevision}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("status bypassed revision guard: %v", err)
	}
	unchanged, err := s.Issue(t.Context(), issue.ID)
	if err != nil || !reflect.DeepEqual(unchanged, issue) {
		t.Fatalf("revision conflict changed issue: %+v %v", unchanged, err)
	}
	if _, err = s.AcquireIssueLock(t.Context(), issue.ID, "other"); err != nil {
		t.Fatal(err)
	}
	var patch model.IssuePatch
	_ = json.Unmarshal([]byte(`{"WorkStatus":"active"}`), &patch)
	if _, err = s.UpdateIssue(t.Context(), "writer", issue.ID, patch); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("lock bypass: %v", err)
	}
	if _, err = s.db.Exec(`UPDATE issues SET work_status='bogus' WHERE id=?`, issue.ID); err == nil {
		t.Fatal("invalid status accepted by database")
	}
}

func TestIssueWorkspaceStatusFilteringAndCIProjection(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := projectionRepository(t, s)
	run := projectionRunWithTrigger(t, s, repo.ID, "feature", projectionSourceSHA, "schedule")
	if _, err := s.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunFailed, Phase: "test", FailedStep: "unit"}); err != nil {
		t.Fatal(err)
	}
	issue, err := s.OpenCIIssue(t.Context(), repo.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	active := model.IssueActive
	issue, err = s.UpdateIssue(t.Context(), "writer", issue.ID, model.IssuePatch{WorkStatus: &active})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcquireIssueLock(t.Context(), issue.ID, "writer"); err != nil {
		t.Fatal(err)
	}
	// Newer unrelated statuses cannot consume a page slot ahead of the active issue.
	for i := 0; i < 3; i++ {
		if _, err = s.CreateManualIssue(t.Context(), "writer", model.ManualIssueSpec{Title: "todo"}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.ListIssues(t.Context(), model.IssueListFilter{WorkStatus: active, Limit: 1})
	if err != nil || len(page.Issues) != 1 || page.Issues[0].ID != issue.ID || page.Total != 1 || page.Counts.CI != 1 || page.NextBefore != 0 {
		t.Fatalf("filter before page/count: %+v %v", page, err)
	}
	now = now.Add(time.Minute)
	next := projectionRunWithTrigger(t, s, repo.ID, "feature", projectionResultSHA, "schedule")
	if _, err = s.FinishRun(t.Context(), next.ID, model.RunResult{Status: model.RunFailed, Phase: "test", FailedStep: "unit"}); err != nil {
		t.Fatal(err)
	}
	repeat, err := s.Issue(t.Context(), issue.ID)
	if err != nil || repeat.WorkStatus != active || repeat.Occurrences != issue.Occurrences+1 || !repeat.UpdatedAt.Equal(now) {
		t.Fatalf("repeat red=%+v %v", repeat, err)
	}
	green := projectionRunWithTrigger(t, s, repo.ID, "feature", projectionBaseSHA, "schedule")
	if _, err = s.FinishRun(t.Context(), green.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	closed, err := s.Issue(t.Context(), issue.ID)
	if err != nil || closed.State != model.IssueClosed || closed.WorkStatus != active {
		t.Fatalf("green closure=%+v %v", closed, err)
	}
}

func TestIssueWorkspaceUpgradeFromV48PreservesIssueAndLock(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	issue, err := s.CreateManualIssue(t.Context(), "writer", model.ManualIssueSpec{Title: "existing", Body: "retained history"})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := s.AcquireIssueLock(t.Context(), issue.ID, "writer")
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the actual v48 shape, then migrate through policy v49, workspace v50 and run-list v51.
	if _, err = s.db.Exec(testdb.PostV48Downgrade); err != nil {
		t.Fatal(err)
	}
	beforeAudit, err := s.VerifyAuditChain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	path := s.livePath
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), path, Options{Now: func() time.Time { return now.Add(time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var ledger string
	if err = s.db.QueryRow(`SELECT group_concat(version, ',') FROM (SELECT version FROM schema_migrations WHERE version>=49 ORDER BY version)`).Scan(&ledger); err != nil || ledger != "49,50,51" {
		t.Fatalf("upgrade ledger=%q, want policy 49, workspace 50 and run-list 51: %v", ledger, err)
	}
	assertSchemaContains(t, s.db, "index", "runs_latest_ref_idx", "status", "started_at", "finished_at", "queued_at", "id")
	got, err := s.Issue(t.Context(), issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkStatus != model.IssueTodo || got.Body != issue.Body || got.State != issue.State || got.Revision != issue.Revision || !got.CreatedAt.Equal(issue.CreatedAt) || !got.UpdatedAt.Equal(issue.UpdatedAt) {
		t.Fatalf("migration changed issue: %+v", got)
	}
	var owner string
	var expires int64
	if err = s.db.QueryRow(`SELECT owner,expires_at FROM issue_locks WHERE issue_id=?`, issue.ID).Scan(&owner, &expires); err != nil || owner != lock.Owner || expires != unixNano(lock.ExpiresAt) {
		t.Fatalf("migration changed lock: %s %d %v", owner, expires, err)
	}
	afterAudit, err := s.VerifyAuditChain(t.Context())
	if err != nil || !reflect.DeepEqual(beforeAudit, afterAudit) {
		t.Fatalf("migration changed audit: %v", err)
	}
	if err = s.migrate(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
}
