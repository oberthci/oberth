package store

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestRunSourcesAreImmutableExactAndSurviveRestart(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run := cancelTestRun(t, s, repo, "feature/sources", "a")
	sources := []pipeline.PinnedSource{{Repository: "forge/acme/producer", Commit: strings.Repeat("b", 40), Path: "/work/sources/producer"}}
	if err := s.RecordRunSources(t.Context(), run.ID, strings.Repeat("f", 40), sources); err == nil {
		t.Fatal("wrong run commit accepted")
	}
	for range 2 {
		if err := s.RecordRunSources(t.Context(), run.ID, run.SHA, sources); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Run(t.Context(), run.ID)
	if err != nil || !reflect.DeepEqual(got.SourceBindings, sources) {
		t.Fatalf("missing durable sources: %+v %v", got.SourceBindings, err)
	}
	changed := append([]pipeline.PinnedSource(nil), sources...)
	changed[0].Commit = strings.Repeat("c", 40)
	if s.RecordRunSources(t.Context(), run.ID, run.SHA, changed) == nil {
		t.Fatal("source receipt rewritten")
	}
	if _, err := s.db.Exec(`UPDATE runs SET source_bindings='[]' WHERE id=?`, run.ID); err == nil {
		t.Fatal("SQL bypassed immutable receipt")
	}
	// A failed unsubmitted run retains its immutable source receipt.
	if _, err := s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverOwnerState(t.Context()); err != nil {
		t.Fatal(err)
	}
	reset, err := s.Run(t.Context(), run.ID)
	if err != nil || reset.ID != run.ID || !reflect.DeepEqual(reset.SourceBindings, sources) {
		t.Fatalf("restart lost sources: %+v %v", reset, err)
	}
}

func TestRunSourcesMigrationPreservesPriorReceipts(t *testing.T) {
	s := newMigrationIDTestStore(t)
	var prior []migration
	for _, m := range migrations {
		if m.id == "" || m.ID() < "20261105180000-pinned-source-bindings" {
			prior = append(prior, m)
		}
	}
	if prior[len(prior)-1].ID() != "20261105050000-execution-target-custody" {
		t.Fatal("test requires exact pre-feature catalog")
	}
	if err := s.migrateWithMigrations(t.Context(), nil, prior); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE saved_source_migration_receipts AS SELECT * FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.migrateWithMigrations(t.Context(), nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
	var changed int
	if err := s.db.QueryRow(`SELECT count(*) FROM saved_source_migration_receipts s LEFT JOIN schema_migrations m ON m.id=s.id WHERE m.id IS NULL OR m.version<>s.version OR m.applied_at<>s.applied_at`).Scan(&changed); err != nil || changed != 0 {
		t.Fatalf("prior receipts changed: %d %v", changed, err)
	}
}
