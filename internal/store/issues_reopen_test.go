package store

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func closedManualIssue(t *testing.T, s *Store) model.Issue {
	t.Helper()
	ctx := context.Background()
	issue, err := s.CreateManualIssue(ctx, "agent@host", model.ManualIssueSpec{Title: "unfinished", Body: "retained history"})
	if err != nil {
		t.Fatal(err)
	}
	closed := model.IssueClosed
	issue, err = s.UpdateManualIssue(ctx, "agent@host", issue.ID, model.IssuePatch{State: &closed})
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

func TestReopenManualIssueAuditAndRetry(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	closed := closedManualIssue(t, s)
	lock, err := s.AcquireIssueLock(ctx, closed.ID, "agent@host")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	got, err := s.ReopenManualIssue(ctx, "agent@host", closed.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := closed
	want.State, want.ClosedAt, want.UpdatedAt = model.IssueOpen, nil, now
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reopen = %#v, want %#v", got, want)
	}
	var expires int64
	if err := s.db.QueryRow(`SELECT expires_at FROM issue_locks WHERE issue_id = ?`, got.ID).Scan(&expires); err != nil || expires != unixNano(now.Add(IssueLockTTL)) || expires <= unixNano(lock.ExpiresAt) {
		t.Fatalf("owner lock not renewed: %d, %v", expires, err)
	}
	var actor, details string
	if err := s.db.QueryRow(`SELECT actor, details FROM audit_actions WHERE action = 'issue.reopen' AND resource_id = ?`, strconv.FormatInt(got.ID, 10)).Scan(&actor, &details); err != nil {
		t.Fatal(err)
	}
	if actor != "agent@host" || details != `{"from":"closed","state":"open"}` {
		t.Fatalf("reopen audit = %q %q", actor, details)
	}
	head, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := s.ReopenManualIssue(ctx, "other@host", got.ID); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("already-open retry ignored another owner's lock: %v", err)
	}
	again, err := s.ReopenManualIssue(ctx, "agent@host", got.ID)
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("retry changed issue = %#v, %v", again, err)
	}
	after, err := s.VerifyAuditChain(ctx)
	if err != nil || !reflect.DeepEqual(after, head) {
		t.Fatalf("retry changed audit = %#v, %v", after, err)
	}
}

func TestReopenManualIssueRollsBackStateAndLockOnAuditFailure(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	closed := closedManualIssue(t, s)
	lock, err := s.AcquireIssueLock(ctx, closed.ID, "agent@host")
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_reopen_audit BEFORE INSERT ON audit_actions WHEN NEW.action = 'issue.reopen' BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := s.ReopenManualIssue(ctx, "agent@host", closed.ID); err == nil {
		t.Fatal("reopen survived an audit failure")
	}
	got, err := s.Issue(ctx, closed.ID)
	if err != nil || !reflect.DeepEqual(got, closed) {
		t.Fatalf("failed reopen changed state = %#v, %v", got, err)
	}
	var expires int64
	if err := s.db.QueryRow(`SELECT expires_at FROM issue_locks WHERE issue_id = ?`, closed.ID).Scan(&expires); err != nil || expires != unixNano(lock.ExpiresAt) {
		t.Fatalf("failed reopen changed lock = %d, %v", expires, err)
	}
	after, err := s.VerifyAuditChain(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed reopen changed audit = %#v, %v", after, err)
	}
}

func TestReopenManualIssueValidationAndExpiredLock(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	closed := closedManualIssue(t, s)
	for _, actor := range []string{"", " \t"} {
		if _, err := s.ReopenManualIssue(ctx, actor, closed.ID); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid actor: %v", err)
		}
	}
	for _, id := range []int64{0, -1} {
		if _, err := s.ReopenManualIssue(ctx, "agent@host", id); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid ID: %v", err)
		}
	}
	if _, err := s.AcquireIssueLock(ctx, closed.ID, "other@host"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(IssueLockTTL)
	if _, err := s.ReopenManualIssue(ctx, "agent@host", closed.ID); err != nil {
		t.Fatalf("expired lock prevented reopen: %v", err)
	}
}
