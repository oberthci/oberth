package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func importedBatch() stateport.IssueImport {
	at := time.Date(2022, 1, 2, 3, 4, 5, 987654321, time.FixedZone("source", 7200))
	r := model.IssueImportRecord{ID: 50, Title: "source private title", Body: "source private body", State: model.IssueOpen,
		CreatedAt: at, UpdatedAt: at, Kind: model.IssueManual, WorkStatus: model.IssueTodo, Occurrences: 1, Author: "source author",
		History: []model.IssueHistory{{Kind: "append", At: at, Author: "source author", Text: "source private comment"}}}
	second := r
	second.ID = 90
	return stateport.IssueImport{Records: []model.IssueImportRecord{r, second}, FileSHA256: strings.Repeat("a", 64)}
}
func importAdmin() stateport.Action {
	return stateport.Action{Actor: "administrator", Caller: actor.Actor{Identity: "administrator", Admin: true}}
}
func issueImportRows(t *testing.T, s *Store) (int, int, int64) {
	t.Helper()
	var count, audit int
	var sequence int64
	requireOK(t, s.readers.QueryRow(`SELECT count(*) FROM issues`).Scan(&count))
	requireOK(t, s.readers.QueryRow(`SELECT count(*) FROM audit`).Scan(&audit))
	requireOK(t, s.readers.QueryRow(`SELECT coalesce((SELECT seq FROM sqlite_sequence WHERE name='issues'),0)`).Scan(&sequence))
	return count, audit, sequence
}

func TestIssueImportDryRunReplayAuditAndSequence(t *testing.T) {
	s := testStore(t)
	q := importedBatch()
	a := importAdmin()
	q.DryRun = true
	_, err := s.ImportIssues(t.Context(), a, q)
	requireOK(t, err)
	n, aud, seq := issueImportRows(t, s)
	if n != 0 || aud != 0 || seq != 0 {
		t.Fatal("dry run wrote state", n, aud, seq)
	}
	q.DryRun = false
	_, err = s.ImportIssues(t.Context(), a, q)
	requireOK(t, err)
	checkSingleImportAudit(t, s, q, a)
	result, err := s.ImportIssues(t.Context(), a, q)
	requireOK(t, err)
	n, aud, seq = issueImportRows(t, s)
	if !result.AlreadyImported || n != 2 || aud != 1 || seq != 90 {
		t.Fatal("replay wrote state", result, n, aud, seq)
	}
	created, err := s.CreateIssue(t.Context(), a, model.Issue{Kind: model.IssueManual, Title: "next"})
	requireOK(t, err)
	if created.ID != 91 {
		t.Fatal("ID continuation lost", created.ID)
	}
}

func checkSingleImportAudit(t *testing.T, s *Store, q stateport.IssueImport, a stateport.Action) {
	t.Helper()
	rows, _, err := s.Audit(t.Context(), 0, 10)
	requireOK(t, err)
	if len(rows) != 1 || rows[0].Action != "issue.import" || rows[0].Actor != a.Actor || !rows[0].CreatedAt.Equal(s.options.Clock()) {
		t.Fatal("import audit shape", rows)
	}
	var details map[string]any
	requireOK(t, json.Unmarshal([]byte(rows[0].Details), &details))
	if len(details) != 2 || details["sha256"] != q.FileSHA256 || details["count"] != float64(2) {
		t.Fatal("audit retained content or lost digest", details)
	}
}

func TestIssueImportRefusesAllConflictsBeforeFirstInsert(t *testing.T) {
	s := testStore(t)
	a := importAdmin()
	_, err := s.CreateIssue(t.Context(), a, model.Issue{Kind: model.IssueManual, Title: "existing"})
	requireOK(t, err)
	q := importedBatch()
	q.Records[1].ID = 1
	// An attempted first insert has a different refusal from the expected ID conflict.
	_, err = s.writer.Exec(`CREATE TRIGGER no_import_insert BEFORE INSERT ON issues BEGIN SELECT RAISE(ABORT,'insert attempted'); END`)
	requireOK(t, err)
	_, err = s.ImportIssues(t.Context(), a, q)
	if err == nil || publicerr.Classify(err).Code() != publicerr.Conflict {
		t.Fatal("did not check all IDs before insert", err)
	}
	n, aud, seq := issueImportRows(t, s)
	if n != 1 || aud != 1 || seq != 1 {
		t.Fatal("conflict wrote state", n, aud, seq)
	}
}

func TestIssueImportRollsBackOnInsertOrAuditFailure(t *testing.T) {
	for _, trigger := range []string{
		`CREATE TRIGGER import_failure BEFORE INSERT ON issues WHEN NEW.id=90 BEGIN SELECT RAISE(ABORT,'second insert refused'); END`,
		`CREATE TRIGGER import_failure BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT,'audit refused'); END`,
	} {
		t.Run(trigger, func(t *testing.T) {
			s := testStore(t)
			_, err := s.writer.Exec(trigger)
			requireOK(t, err)
			if _, err = s.ImportIssues(t.Context(), importAdmin(), importedBatch()); err == nil {
				t.Fatal("failure accepted")
			}
			if !strings.Contains(strings.Join(publicerr.Classify(err).Context, " "), "No issues imported.") {
				t.Fatal("rollback outcome missing", err)
			}
			n, aud, seq := issueImportRows(t, s)
			if n != 0 || aud != 0 || seq != 0 {
				t.Fatal("transaction leaked state", n, aud, seq)
			}
		})
	}
}

func TestIssueImportStoreRequiresExplicitAdministrator(t *testing.T) {
	for _, a := range []stateport.Action{{Actor: "internal"}, {Actor: "x", Caller: actor.Actor{Identity: "x"}}, {Actor: "x", Caller: actor.Actor{Identity: "x", Admin: true, Scope: []string{"origin"}}}, {Actor: "x", Caller: actor.Actor{Identity: "y", Admin: true}}} {
		s := testStore(t)
		if _, err := s.ImportIssues(t.Context(), a, importedBatch()); err == nil {
			t.Fatal("non-administrator accepted")
		}
		n, aud, seq := issueImportRows(t, s)
		if n != 0 || aud != 0 || seq != 0 {
			t.Fatal("refusal wrote state")
		}
	}
}

func TestIssueCreateImportAndProjectionWriteCompleteValueOnce(t *testing.T) {
	s := testStore(t)
	for _, sql := range []string{
		`CREATE TABLE issue_writes(kind TEXT)`,
		`CREATE TRIGGER count_issue_insert AFTER INSERT ON issues BEGIN INSERT INTO issue_writes VALUES('insert'); END`,
		`CREATE TRIGGER count_issue_update AFTER UPDATE ON issues BEGIN INSERT INTO issue_writes VALUES('update'); END`,
		`CREATE TRIGGER require_complete_issue BEFORE INSERT ON issues WHEN json_extract(NEW.value,'$.id') != NEW.id BEGIN SELECT RAISE(ABORT,'incomplete issue insert'); END`,
	} {
		_, err := s.writer.ExecContext(t.Context(), sql)
		requireOK(t, err)
	}
	_, err := s.CreateIssue(t.Context(), importAdmin(), model.Issue{Kind: model.IssueManual, Title: "native"})
	requireOK(t, err)
	_, err = s.ImportIssues(t.Context(), importAdmin(), importedBatch())
	requireOK(t, err)
	run, _ := accepted(t, s, "write-once")
	requireOK(t, s.Finish(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunUpdate{ID: run.ID, Revision: run.Revision, Status: stepmodel.RunFailed, ProjectCI: true}))
	var inserts, updates int
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM issue_writes WHERE kind='insert'`).Scan(&inserts))
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM issue_writes WHERE kind='update'`).Scan(&updates))
	if inserts != 4 || updates != 0 {
		t.Fatal("new issues wrote their full value more than once", inserts, updates)
	}
}
