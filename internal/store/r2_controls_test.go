package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"modernc.org/sqlite"
)

func TestBatchRejectsMixedRepositoriesBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	batch := batchFixture(t, s)
	_, err := s.RegisterRepository(t.Context(), stateport.Action{Actor: "operator"}, "origin/team/other")
	requireOK(t, err)
	input := &batch.Runs[1]
	input.Request.Repo = "origin/team/other"
	view := input.Prepared.View()
	view.Repository = input.Request.Repo
	input.Prepared = sealView(t, view)
	before := databaseContents(t, s.readers)
	_, err = s.AcceptBatch(t.Context(), batch)
	requireCode(t, err, publicerr.InvalidInput)
	if before != databaseContents(t, s.readers) {
		t.Fatal("mixed repositories wrote batch")
	}
}

func TestBusyStatementReportsWriterLock(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	original := s.writer
	// A deferred writer lets BEGIN succeed while its INSERT encounters a real lock.
	deferred, err := sql.Open("sqlite", "file:"+s.path+"?_pragma=busy_timeout(0)")
	requireOK(t, err)
	deferred.SetMaxOpenConns(1)
	s.writer = deferred
	defer func() { s.writer = original; requireOK(t, deferred.Close()) }()
	held, err := original.BeginTx(t.Context(), nil)
	requireOK(t, err)
	defer func() { requireOK(t, held.Rollback()) }()
	_, err = s.CreateIssue(t.Context(), stateport.Action{Actor: "operator"}, model.Issue{Kind: model.IssueManual, Title: "locked"})
	requireSQLiteDiagnostic(t, err, 5, locked)
}

func TestFullSQLiteVolumeReportsDiskCauseWithoutWriting(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	var pages int64
	requireOK(t, s.writer.QueryRowContext(t.Context(), `PRAGMA page_count`).Scan(&pages))
	_, err := s.writer.ExecContext(t.Context(), fmt.Sprintf(`PRAGMA max_page_count=%d`, pages+1))
	requireOK(t, err)
	before := databaseContents(t, s.readers)
	_, err = s.CreateIssue(t.Context(), stateport.Action{Actor: "operator"}, model.Issue{Kind: model.IssueManual, Title: "full", Body: strings.Repeat("x", 96<<10)})
	requireSQLiteDiagnostic(t, err, 13, diskFull)
	if before != databaseContents(t, s.readers) {
		t.Fatal("full volume left partial fact")
	}
}

func requireSQLiteDiagnostic(t *testing.T, err error, code int, kind refusal) {
	t.Helper()
	var database *sqlite.Error
	var public *publicerr.Error
	_, reason, remedy := message(kind)
	if !errors.As(err, &database) || database.Code()&255 != code || !errors.As(err, &public) || public.Reason() != reason || public.Remedy() != remedy {
		t.Fatal("wrong SQLite cause", err)
	}
	requireCode(t, err, publicerr.Unavailable)
}

func TestAuditedFactValidatesEventsBeforeStorage(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"kind", "summary", "data"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			event := atomicEvent(s, "bad")
			switch mode {
			case "kind":
				event.Event.Kind = "unknown"
			case "summary":
				event.Event.Summary = strings.Repeat("x", model.MaxEventSummaryRunes+1)
			case "data":
				event.Event.Data = []byte(`not JSON`)
			}
			before := databaseContents(t, s.readers)
			err := s.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator", Events: []stateport.EventWrite{event}}, stateport.Upstream{Name: "origin", Org: "team", Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"})
			requireCode(t, err, publicerr.InvalidInput)
			if before != databaseContents(t, s.readers) {
				t.Fatal("invalid event or its fact was stored")
			}
		})
	}
}

func TestSecretGrantsFiltersRepositoryBeforeLimit(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	atomicRepository(t, s)
	_, err := s.RegisterRepository(t.Context(), stateport.Action{Actor: "operator"}, "origin/team/other")
	requireOK(t, err)
	for _, repo := range []string{"origin/team/repo", "origin/team/other"} {
		requireOK(t, s.SecretGrant(t.Context(), stateport.Action{Actor: "operator"}, workflowpolicy.GrantKey{Key: identityname.Key{Repository: repo, File: "ci.yaml", Step: "build"}, Path: "token", Audience: "login"}, false))
	}
	for _, scope := range [][]string{nil, {"origin/team/repo"}, {}} {
		rows, err := s.SecretGrants(t.Context(), stateport.CatalogFilter{Repositories: scope, Limit: 1})
		requireOK(t, err)
		if len(scope) == 1 && (len(rows) != 1 || rows[0].Repository != scope[0]) {
			t.Fatal("grant scope leaked", rows)
		}
		if scope != nil && len(scope) == 0 && len(rows) != 0 {
			t.Fatal("empty grant scope widened", rows)
		}
		if scope == nil && len(rows) != 1 {
			t.Fatal("unscoped grant page missing")
		}
	}
}

func TestRejectedAdmissionRequiresFindingsAndBoundedReason(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 80, 81} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s := testStore(t)
			input := rejectedFixture(t, s)
			input.Rejected.Findings = nil
			if n != 0 {
				input.Rejected.Findings = []admissioncontract.Finding{{Code: "refused", Reason: strings.Repeat("r", n), Remedy: "Fix the pipeline."}}
			}
			before := databaseContents(t, s.readers)
			_, err := acceptForTest(t.Context(), s, input)
			if n == 80 {
				requireOK(t, err)
				return
			}
			requireCode(t, err, publicerr.InvalidInput)
			if before != databaseContents(t, s.readers) {
				t.Fatal("invalid rejection wrote facts")
			}
		})
	}
}

func TestRejectedAdmissionRollsBackAuditReceiptAndEvents(t *testing.T) {
	t.Parallel()
	checkAtomicFacts(t, []atomicFact{{"run.reject", func(t *testing.T, s *Store) func(stateport.Action) error {
		input := rejectedFixture(t, s)
		return func(a stateport.Action) error {
			input.Events = a.Events
			_, err := acceptForTest(t.Context(), s, input)
			return err
		}
	}}})
}
