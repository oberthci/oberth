package store

import (
	"errors"
	"github.com/oberthci/oberth/internal/model"
	"testing"
	"time"
)

func TestIssueRevisionChangesOnlyForRealMetadataChanges(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	issue, err := s.CreateManualIssue(t.Context(), "writer", model.ManualIssueSpec{Title: "initial", Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		sql      string
		revision int64
	}{
		{`UPDATE issues SET title=title,state=state,scope_upstream_id=scope_upstream_id WHERE id=?`, 1},
		{`UPDATE issues SET body='changed',updated_at=updated_at+1 WHERE id=?`, 1},
		{`UPDATE issues SET title='changed' WHERE id=?`, 2},
		{`UPDATE issues SET state='closed' WHERE id=?`, 3},
		{`UPDATE issues SET state='open' WHERE id=?`, 4},
	} {
		if _, err := s.db.ExecContext(t.Context(), tc.sql, issue.ID); err != nil {
			t.Fatal(err)
		}
		var revision int64
		if err := s.db.QueryRowContext(t.Context(), `SELECT revision FROM issues WHERE id=?`, issue.ID).Scan(&revision); err != nil {
			t.Fatal(err)
		}
		if revision != tc.revision {
			t.Fatalf("%s: revision=%d want %d", tc.sql, revision, tc.revision)
		}
	}
}

func TestIssueRevisionMismatchRollsBackLockAndAudit(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	issue, err := s.CreateManualIssue(t.Context(), "writer", model.ManualIssueSpec{Title: "initial", Body: "original"})
	if err != nil {
		t.Fatal(err)
	}
	title := "committed"
	if _, err = s.UpdateIssue(t.Context(), "writer", issue.ID, model.IssuePatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcquireIssueLock(t.Context(), issue.ID, "writer"); err != nil {
		t.Fatal(err)
	}
	var beforeAudit, beforeExpiry int64
	if err = s.db.QueryRow(`SELECT count(*) FROM audit_actions`).Scan(&beforeAudit); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT expires_at FROM issue_locks WHERE issue_id=?`, issue.ID).Scan(&beforeExpiry); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	expected := int64(1)
	title = "stale"
	body := "must not append"
	if _, err = s.UpdateIssue(t.Context(), "writer", issue.ID, model.IssuePatch{Title: &title, Body: &body, ExpectedRevision: &expected}); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	var afterAudit, afterExpiry int64
	if err = s.db.QueryRow(`SELECT count(*) FROM audit_actions`).Scan(&afterAudit); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT expires_at FROM issue_locks WHERE issue_id=?`, issue.ID).Scan(&afterExpiry); err != nil {
		t.Fatal(err)
	}
	if beforeAudit != afterAudit || beforeExpiry != afterExpiry {
		t.Fatalf("conflict changed audit/lock: %d/%d %d/%d", beforeAudit, afterAudit, beforeExpiry, afterExpiry)
	}
}
