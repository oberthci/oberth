package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRunIssueTrailersPersistOnceWithCommitBinding(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run := cancelTestRun(t, s, repo, "feature/trailers", "a")
	if run.IssueTrailersRecorded || len(run.IssueTrailers) != 0 {
		t.Fatal("unknown history fabricated trailers")
	}
	trailers := []model.CommitIssueTrailer{{IssueID: 1478, SHA: run.SHA, Close: true}}
	if err := s.RecordRunIssueTrailers(t.Context(), run.ID, run.SHA, trailers); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got, err := s.Run(t.Context(), run.ID)
		if err != nil || !got.IssueTrailersRecorded || !reflect.DeepEqual(got.IssueTrailers, trailers) {
			t.Fatalf("receipt: %+v %v", got, err)
		}
		if err := s.RecordRunIssueTrailers(t.Context(), run.ID, run.SHA, trailers); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordRunIssueTrailers(t.Context(), run.ID, run.SHA, nil); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("overwrite: %v", err)
	}
	if err := s.RecordRunIssueTrailers(t.Context(), run.ID, strings.Repeat("b", 40), trailers); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong commit: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE runs SET issue_trailers='[]' WHERE id=?`, run.ID); err == nil {
		t.Fatal("SQL rewrote immutable receipt")
	}
	noTrailers := cancelTestRun(t, s, repo, "feature/plain", "b")
	if err := s.RecordRunIssueTrailers(t.Context(), noTrailers.ID, noTrailers.SHA, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Run(t.Context(), noTrailers.ID)
	if err != nil || !got.IssueTrailersRecorded || len(got.IssueTrailers) != 0 {
		t.Fatalf("empty receipt: %+v %v", got, err)
	}
}

func TestRunIssueTrailersTimestampMigrationPreservesRowsAndReceipts(t *testing.T) {
	s := newMigrationIDTestStore(t)
	const id = "20261104160000-run-issue-trailers"
	old := make([]migration, 0, len(migrations))
	for _, m := range migrations {
		if m.ID() != id {
			old = append(old, m)
		}
	}
	if err := s.migrateWithMigrations(t.Context(), nil, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE saved_receipts AS SELECT * FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateWithMigrations(t.Context(), nil, migrations); err != nil {
		t.Fatal(err)
	}
	var changed int
	if err := s.db.QueryRow(`SELECT count(*) FROM saved_receipts s LEFT JOIN schema_migrations m ON m.id=s.id WHERE m.id IS NULL OR m.version<>s.version OR m.applied_at<>s.applied_at`).Scan(&changed); err != nil || changed != 0 {
		t.Fatalf("old receipts changed: %d %v", changed, err)
	}
	if err := s.migrateWithMigrations(t.Context(), nil, migrations); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('runs') WHERE name='issue_trailers' AND dflt_value="''"`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("column: %d %v", count, err)
	}
}

func TestFailureSummaryPreservesAutomaticRetryLineage(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run := cancelTestRun(t, s, repo, "feature/failure-summary", "c")
	origin := strings.Repeat("a", 32)
	reason := "automatic retry of " + origin + " after infrastructure failure: workflow unavailable"
	if _, err := s.db.Exec(`UPDATE runs SET reason=? WHERE id=?`, reason, run.ID); err != nil {
		t.Fatal(err)
	}
	result, err := s.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunFailed, Phase: "infrastructure", Error: "full raw failure", FailureCause: "infrastructure error: cpu limit 6 over ceiling 4"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != reason || result.AutoRetryOf != origin || result.FailureCause != "infrastructure error: cpu limit 6 over ceiling 4" || result.Error != "full raw failure" {
		t.Fatalf("summary changed lineage/evidence: %+v", result)
	}
}
