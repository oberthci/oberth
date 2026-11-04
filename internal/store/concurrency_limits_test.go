package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestConcurrencyGroupLimitPersistsAndRejectsConflictingYAML(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	other, err := s.CreateRepository(t.Context(), model.RepositorySpec{Name: "other", UpstreamID: repo.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "first", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetRunConcurrencyGroup(t.Context(), run.ID, "state", 2); err != nil {
		t.Fatal(err)
	}
	got, err := s.Run(t.Context(), run.ID)
	if err != nil || got.ConcurrencyLimit != 2 {
		t.Fatal(got, err)
	}
	if err = s.SetRunConcurrencyGroup(t.Context(), run.ID, "state", 1); err == nil {
		t.Fatal("changed immutable run limit")
	}
	if _, err = s.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	path := s.livePath
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenAdminClient(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	next, err := s.EnqueueRun(t.Context(), testRunSpec(other.ID, "second", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetRunConcurrencyGroup(t.Context(), next.ID, "state", 1); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatal("conflicting YAML accepted after restart", err)
	}
	if err = s.SetRunConcurrencyGroup(t.Context(), next.ID, "state", 2); err != nil {
		t.Fatal(err)
	}
	groups, err := s.ConcurrencyGroups(t.Context())
	if err != nil || len(groups) != 1 || groups[0].Limit != 2 {
		t.Fatal(groups, err)
	}
}

func TestConcurrencyGroupMigrationPreservesLegacyGroupAndLedger(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "main", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetRunConcurrencyGroup(t.Context(), run.ID, "state"); err != nil {
		t.Fatal(err)
	}
	for _, status := range []model.RunStatus{model.RunPassed, model.RunFailed, model.RunInterrupted} {
		idleRepo, err := s.CreateRepository(t.Context(), model.RepositorySpec{Name: "idle-" + string(status), UpstreamID: repo.UpstreamID, DefaultBranch: "main"})
		if err != nil {
			t.Fatal(err)
		}
		idle, err := s.EnqueueRun(t.Context(), testRunSpec(idleRepo.ID, "old", strings.Repeat("b", 40)))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetRunConcurrencyGroup(t.Context(), idle.ID, "retired"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FinishRun(t.Context(), idle.ID, model.RunResult{Status: status}); err != nil {
			t.Fatal(err)
		}
		// A later ungrouped run must not revive an older terminal declaration.
		newer, err := s.EnqueueRun(t.Context(), testRunSpec(idleRepo.ID, "new", strings.Repeat("c", 40)))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetRunConcurrencyGroup(t.Context(), newer.ID, "", 1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FinishRun(t.Context(), newer.ID, model.RunResult{Status: model.RunPassed}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec(`DROP TRIGGER run_issue_trailers_immutable; ALTER TABLE runs DROP COLUMN issue_trailers; ALTER TABLE runs DROP COLUMN failure_cause; DROP TABLE deferred_branch_receives; DROP TABLE queue_cap; DROP TABLE concurrency_group_declarations; ALTER TABLE runs DROP COLUMN concurrency_limit; DELETE FROM schema_migrations WHERE id IN ('20261104120123-concurrency-limits-971dc11f4a5530e149fd22d0d6847b46','20261104130000-queue-cap','20261104160000-run-issue-trailers','20261104190000-priority-four-classes')`); err != nil {
		t.Fatal(err)
	}
	before := releaseSplitLedger(t, s.db)
	if err = s.migrate(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	after := releaseSplitLedger(t, s.db)
	for id, at := range before {
		if after[id] != at {
			t.Fatal("old receipt changed", id)
		}
	}
	got, err := s.Run(t.Context(), run.ID)
	if err != nil || got.ConcurrencyGroup != "state" || got.ConcurrencyLimit != 1 {
		t.Fatal(got, err)
	}
	groups, err := s.ConcurrencyGroups(t.Context())
	if err != nil || len(groups) != 1 || groups[0].Limit != 1 {
		t.Fatal(groups, err)
	}
	if err = s.migrate(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	next, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "new-limit", strings.Repeat("d", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunConcurrencyGroup(t.Context(), next.ID, "retired", 2); err != nil {
		t.Fatalf("terminal history blocked another repository's new limit: %v", err)
	}
}

func TestConcurrencyGroupYAMLRemovalClearsIdleDefinition(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	first, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "first", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetRunConcurrencyGroup(t.Context(), first.ID, "state", 2); err != nil {
		t.Fatal(err)
	}
	second, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "second", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetRunConcurrencyGroup(t.Context(), second.ID, "", 1); err != nil {
		t.Fatal(err)
	}
	groups, err := s.ConcurrencyGroups(t.Context())
	if err != nil || len(groups) != 0 {
		t.Fatal("YAML removal did not clear definition", groups, err)
	}
	third, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "third", strings.Repeat("c", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetRunConcurrencyGroup(t.Context(), third.ID, "state", 3); err == nil {
		t.Fatal("changed limit while old member unfinished")
	}
	if _, err = s.FinishRun(t.Context(), first.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetRunConcurrencyGroup(t.Context(), third.ID, "state", 3); err != nil {
		t.Fatal(err)
	}
}
