package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestIssueLifecycleLockVisibilityAndBreak(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)
	issue, err := s.CreateManualIssue(ctx, "owner", model.ManualIssueSpec{Title: "locked"})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := s.AcquireIssueLock(ctx, issue.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ActiveIssueLock(ctx, issue.ID)
	if err != nil || got == nil || got.Owner != "owner" || !got.ExpiresAt.Equal(lock.ExpiresAt) {
		t.Fatalf("issue=%+v %v", got, err)
	}
	stored, err := s.Issue(ctx, issue.ID)
	if err != nil || stored.Lock != nil {
		t.Fatalf("stored issue includes transient lock=%+v %v", stored, err)
	}
	page, err := s.ListIssues(ctx, model.IssueListFilter{})
	if err != nil || len(page.Issues) != 1 || page.Issues[0].Lock != nil {
		t.Fatalf("page=%+v %v", page, err)
	}
	_, err = s.AppendIssueBody(ctx, "other", issue.ID, "append")
	var held *IssueLockHeldError
	if !errors.As(err, &held) || !strings.Contains(err.Error(), "owner") || !strings.Contains(err.Error(), lock.ExpiresAt.Format(time.RFC3339Nano)) {
		t.Fatalf("conflict=%v", err)
	}
	if err := s.BreakIssueLock(ctx, "admin", issue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewIssueLock(ctx, issue.ID, "owner"); !errors.Is(err, ErrLockNotOwned) {
		t.Fatalf("former owner renewed: %v", err)
	}
	got, err = s.ActiveIssueLock(ctx, issue.ID)
	if err != nil || got != nil {
		t.Fatalf("lock after break=%+v %v", got, err)
	}
	var detail string
	if err := s.db.QueryRowContext(ctx, `SELECT details FROM audit_actions WHERE action='issue.lock.break'`).Scan(&detail); err != nil || !strings.Contains(detail, `"previous_owner":"owner"`) {
		t.Fatalf("audit=%s %v", detail, err)
	}
	if _, err := s.AcquireIssueLock(ctx, issue.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(IssueLockTTL + time.Second)
	got, err = s.ActiveIssueLock(ctx, issue.ID)
	if err != nil || got != nil {
		t.Fatalf("expired lock=%+v %v", got, err)
	}
}

func TestIssueCloseReasonIsAtomicAndLockAware(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)
	issue, err := s.CreateManualIssue(ctx, "owner", model.ManualIssueSpec{Title: "issue", Body: "original"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireIssueLock(ctx, issue.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseIssueWithReason(ctx, "admin", issue.ID, "obsolete"); !errors.Is(err, ErrLockHeld) {
		t.Fatal(err)
	}
	if err := s.BreakIssueLock(ctx, "admin", issue.ID); err != nil {
		t.Fatal(err)
	}
	closed, err := s.CloseIssueWithReason(ctx, "admin", issue.ID, "obsolete")
	if err != nil || closed.State != model.IssueClosed || closed.Body != "original\n\nobsolete" {
		t.Fatalf("closed=%+v %v", closed, err)
	}
	if _, err := s.CloseIssueWithReason(ctx, "admin", issue.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_actions WHERE action='issue.close' AND details LIKE '%obsolete%'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit=%d %v", count, err)
	}
}

func TestIssueCloseReasonUsesCICloseAudit(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	issue, err := s.UpsertCIIssue(ctx, "owner", repo.ID, "main", "CI failed", "failure")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseIssueWithReason(ctx, "admin", issue.ID, "resolved"); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("locked CI close = %v", err)
	}
	if err := s.BreakIssueLock(ctx, "admin", issue.ID); err != nil {
		t.Fatal(err)
	}
	closed, err := s.CloseIssueWithReason(ctx, "admin", issue.ID, "resolved")
	if err != nil || closed.Kind != model.IssueCI || closed.State != model.IssueClosed || closed.ClosedAt == nil || closed.Body != "failure\n\nresolved" {
		t.Fatalf("closed CI issue = %+v, %v", closed, err)
	}
	if _, err := s.CloseIssueWithReason(ctx, "admin", issue.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	var action, raw string
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_actions WHERE action IN ('issue.close','issue.ci.close')`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("close audit count = %d, %v", count, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT action,details FROM audit_actions WHERE action IN ('issue.close','issue.ci.close')`).Scan(&action, &raw); err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatal(err)
	}
	if action != "issue.ci.close" || detail["branch"] != "main" || detail["resolution"] != "resolved" {
		t.Fatalf("CI close audit = %s %s", action, raw)
	}
	if _, err := s.CloseCIIssue(ctx, "admin", issue.ID, "stale close"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale CI close = %v", err)
	}
	stored, err := s.Issue(ctx, issue.ID)
	if err != nil || stored.Body != closed.Body {
		t.Fatalf("retry changed CI body = %+v, %v", stored, err)
	}
}
