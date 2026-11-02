package store

import (
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
