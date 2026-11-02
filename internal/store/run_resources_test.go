package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRunResourcesReadHistoryRefFailedAndRepoBounds(t *testing.T) {
	at := time.Now().UTC()
	s := testStore(t, &at)
	repo := createRepo(t, s)
	var ids []string
	for i, ref := range []string{"main", "feature", "main"} {
		run, e := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, ref, strings.Repeat(string(rune('a'+i)), 40)))
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, run.ID)
		if _, e = s.SetRunWorkflowName(t.Context(), run.ID, "workflow-test"); e != nil {
			t.Fatal(e)
		}
		v := resourceRow(run.ID, at)
		if e = s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{v}); e != nil {
			t.Fatal(e)
		}
		status := "passed"
		if i == 2 {
			status = "failed"
		}
		if _, e = s.db.Exec(`UPDATE runs SET status=? WHERE id=?`, status, run.ID); e != nil {
			t.Fatal(e)
		}
	}
	got, e := s.RunResources(t.Context(), repo.ID, RunResourceFilter{Ref: "main", Runs: 30})
	if e != nil || len(got.Runs) != 1 || got.Runs[0].ID != ids[0] || len(got.Observations) != 1 {
		t.Fatalf("default %v %v", got, e)
	}
	got, e = s.RunResources(t.Context(), repo.ID, RunResourceFilter{Ref: "any", Runs: 2, IncludeFailed: true})
	if e != nil || len(got.Runs) != 2 || got.Runs[0].ID != ids[2] {
		t.Fatalf("any %v %v", got, e)
	}
	got, e = s.RunResources(t.Context(), repo.ID+1, RunResourceFilter{RunID: ids[0], Runs: 1, IncludeFailed: true})
	if e != nil || len(got.Runs) != 0 || len(got.Observations) != 0 {
		t.Fatal("cross repo", e)
	}
	if _, e = s.RunResources(t.Context(), repo.ID, RunResourceFilter{Runs: 101}); e == nil {
		t.Fatal("unbounded")
	}
}

func TestRunResourcesUsesIndexedReadPoolWithoutWriterTransaction(t *testing.T) {
	at := time.Now().UTC()
	s := testStore(t, &at)
	rows, e := s.reader().Query(`EXPLAIN QUERY PLAN `+runResourceObservationSQL, `["run"]`, 1, 10)
	if e != nil {
		t.Fatal(e)
	}
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if e := rows.Scan(&id, &parent, &unused, &detail); e != nil {
			t.Fatal(e)
		}
		plan.WriteString(detail)
	}
	e = rows.Err()
	_ = rows.Close()
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(plan.String(), "SEARCH u USING INDEX sqlite_autoindex_step_resource_usage_1 (run_id=?)") || strings.Contains(plan.String(), "step_resource_usage_repo_idx") {
		t.Fatal(plan.String())
	}
	// Holding the write connection must not make a history report wait for
	// that connection; readers use the separate WAL read pool.
	tx, e := s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = tx.Rollback() }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, e := s.RunResources(ctx, 1, RunResourceFilter{Ref: "any", Runs: 1}); e != nil {
		t.Fatal(e)
	}
}

func TestRunResourcesFiltersWorkflowClassBeforeLimit(t *testing.T) {
	at := time.Now().UTC()
	s := testStore(t, &at)
	repo := createRepo(t, s)
	ids := make(map[string]string)
	for i, class := range []string{"release", "promotion", "schedule", "branch"} {
		run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, class, strings.Repeat(string(rune('a'+i)), 40)))
		if err != nil {
			t.Fatal(err)
		}
		release, trigger := 0, class
		if class == "release" {
			release, trigger = 1, "promotion"
		}
		if _, err := s.db.Exec(`UPDATE runs SET status='passed',release=?,trigger=? WHERE id=?`, release, trigger, run.ID); err != nil {
			t.Fatal(err)
		}
		ids[class] = run.ID
	}
	// A busy branch history must not evict the last release before class filtering.
	for i := 0; i < 35; i++ {
		run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "busy", strings.Repeat("e", 40)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE runs SET status='passed' WHERE id=?`, run.ID); err != nil {
			t.Fatal(err)
		}
		ids["branch"] = run.ID
	}
	for _, class := range []string{"release", "promotion", "schedule", "branch"} {
		got, err := s.RunResources(t.Context(), repo.ID, RunResourceFilter{Ref: "any", Runs: 1, DurationClass: class})
		if err != nil || len(got.Runs) != 1 || got.Runs[0].ID != ids[class] {
			t.Fatalf("%s history = %+v, %v; want %s", class, got.Runs, err, ids[class])
		}
	}
}
