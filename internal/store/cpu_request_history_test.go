package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestCPURequestHistoryIsBoundedByRepositoryClassAndRecentRuns(t *testing.T) {
	at := time.Now().UTC()
	s := testStore(t, &at)
	repo := createRepo(t, s)
	for i := range 130 {
		spec := testRunSpec(repo.ID, fmt.Sprintf("cpu/%d", i), strings.Repeat("a", 40))
		if i == 129 {
			spec.Trigger = "promotion"
		}
		run, err := s.EnqueueRun(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.SetRunWorkflowName(t.Context(), run.ID, "workflow-test"); err != nil {
			t.Fatal(err)
		}
		row := resourceRow(run.ID, at)
		if err = s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{row}); err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec(`UPDATE runs SET status='passed',started_at=? WHERE id=?`, unixNano(at), run.ID); err != nil {
			t.Fatal(err)
		}
		at = at.Add(time.Second)
	}
	rows, err := s.CPURequestHistory(t.Context(), repo.ID, "branch")
	if err != nil || len(rows) != 127 {
		t.Fatalf("recent 128-run window: %d %v", len(rows), err)
	}
	rows, err = s.CPURequestHistory(t.Context(), repo.ID, "promotion")
	if err != nil || len(rows) != 1 {
		t.Fatalf("class scope: %d %v", len(rows), err)
	}
	rows, err = s.CPURequestHistory(t.Context(), repo.ID+1, "branch")
	if err != nil || len(rows) != 0 {
		t.Fatal("cross repository history", rows, err)
	}
	plan, err := s.reader().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+cpuRequestHistorySQL, repo.ID, "branch")
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	var details strings.Builder
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details.WriteString(detail)
	}
	if err = plan.Err(); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"runs_repo_started_idx", "sqlite_autoindex_step_resource_usage_1"} {
		if !strings.Contains(details.String(), index) {
			t.Fatal("history lookup lost bounded index", details.String())
		}
	}
}
