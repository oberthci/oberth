package store

import (
	"context"
	"github.com/oberthci/oberth/internal/publicerr"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestIssueImportRefusesExistingWithoutChangingIt(t *testing.T) {
	s := testStore(t)
	a := importAdmin()
	existing, err := s.CreateIssue(t.Context(), a, model.Issue{Kind: model.IssueManual, Title: "original", Body: "original body"})
	requireOK(t, err)
	q := importedBatch()
	q.Records[0].ID = existing.ID
	if _, err = s.ImportIssues(t.Context(), a, q); err == nil {
		t.Fatal("overwrite was allowed")
	}
	got, err := s.Issue(t.Context(), existing.ID)
	requireOK(t, err)
	if got.Title != "original" || got.Body != "original body" || got.Imported != nil {
		t.Fatal("existing record changed")
	}
	n, aud, seq := issueImportRows(t, s)
	if n != 1 || aud != 1 || seq != existing.ID {
		t.Fatal("conflict changed state")
	}
}

func TestIssueImportRefusesInvalidBatchWithoutWriting(t *testing.T) {
	for _, mutate := range []func(*stateport.IssueImport){
		func(q *stateport.IssueImport) { q.FileSHA256 = "bad" },
		func(q *stateport.IssueImport) { q.Records[1].ID = q.Records[0].ID },
		func(q *stateport.IssueImport) { q.Records[1].ID = 0 },
		func(q *stateport.IssueImport) { q.Records[1].History[0].Kind = "authority" },
	} {
		s := testStore(t)
		q := importedBatch()
		mutate(&q)
		if _, err := s.ImportIssues(t.Context(), importAdmin(), q); err == nil {
			t.Fatal("invalid batch accepted")
		}
		n, aud, seq := issueImportRows(t, s)
		if n != 0 || aud != 0 || seq != 0 {
			t.Fatal("invalid batch wrote state")
		}
	}
}

func TestIssueImportProvenanceSurvivesReopen(t *testing.T) {
	s := testStore(t)
	a := importAdmin()
	q := importedBatch()
	_, err := s.ImportIssues(t.Context(), a, q)
	requireOK(t, err)
	issue, err := s.Issue(t.Context(), 50)
	requireOK(t, err)
	if issue.Imported == nil || issue.Author != "source author" {
		t.Fatal("provenance missing")
	}
	_, err = s.UpdateIssue(t.Context(), a, model.IssuePatch{ID: 50, Title: new("local edit")})
	requireOK(t, err)
	other, err := Open(t.Context(), s.path, s.options)
	requireOK(t, err)
	defer func() { requireOK(t, other.Close()) }()
	r, err := other.ImportIssues(t.Context(), a, q)
	requireOK(t, err)
	if !r.AlreadyImported {
		t.Fatal("restart lost receipt")
	}
	got, err := other.Issue(t.Context(), 50)
	requireOK(t, err)
	if got.Title != "local edit" || len(got.History) != 1 || got.Imported == nil {
		t.Fatal("restart changed source history")
	}
	var keys, credentials int
	requireOK(t, s.readers.QueryRow(`SELECT count(*) FROM keys`).Scan(&keys))
	requireOK(t, s.readers.QueryRow(`SELECT count(*) FROM credentials`).Scan(&credentials))
	if keys != 0 || credentials != 0 {
		t.Fatal("source identities gained authority")
	}
}

func TestIssueImportCancelledBeforeAuditConfirmsRollback(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	now := s.options.Clock()
	s.options.Clock = func() time.Time { cancel(); return now }
	_, err := s.ImportIssues(ctx, importAdmin(), importedBatch())
	if err == nil || !strings.Contains(strings.Join(publicerr.Classify(err).Context, " "), "No issues imported.") {
		t.Fatal("missing rollback outcome", err)
	}
	n, a, seq := issueImportRows(t, s)
	if n != 0 || a != 0 || seq != 0 {
		t.Fatal("cancelled import leaked state", n, a, seq)
	}
	s.options.Clock = func() time.Time { return now }
	result, err := s.ImportIssues(t.Context(), importAdmin(), importedBatch())
	requireOK(t, err)
	if result.AlreadyImported {
		t.Fatal("cancelled transaction left a receipt")
	}
}
