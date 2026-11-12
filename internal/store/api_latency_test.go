package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestAPILatencyQueries(t *testing.T) {
	s := latestRefLargeFixture(t)
	ctx := t.Context()
	// Historical steps must not turn page enrichment into a join over all steps.
	_, err := s.db.ExecContext(ctx, `INSERT INTO step_results(run_id,burn,step,ordinal,status,exit_code,log_start,log_end,started_at,finished_at,recorded_at)
 SELECT id,'build','test',0,'passed',0,0,100,1,2,2 FROM runs`)
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"latest": func() error {
			_, err := s.ListRecentRuns(ctx, model.RunListFilter{LatestPerRef: true, Limit: 100})
			return err
		},
		"queue_cap": func() error { _, err := s.QueueCap(ctx); return err },
		"counts":    func() error { _, err := s.CountRuns(ctx); return err },
	} {
		start := time.Now()
		if err := call(); err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(start)
		t.Logf("%s: %s", name, elapsed)
		if elapsed >= 500*time.Millisecond {
			t.Fatalf("%s exceeded 500ms", name)
		}
	}
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+latestRefQuery("", priorityOrderSQL(s.now())), "", MaxActiveRuns, "", 0, 0, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "SCAN") || strings.Contains(detail, "TEMP") {
			t.Log(detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestAPIQueryPlansUseHistoryIndexes(t *testing.T) {
	s := latestRefLargeFixture(t)
	checks := []struct {
		name, query, index string
		args               []any
	}{
		{"counts", `SELECT repo_id,COUNT(*),SUM(status='queued' OR (status='running' AND started_at IS NULL)),SUM(status='running' AND started_at IS NOT NULL) FROM runs GROUP BY repo_id`, "COVERING INDEX runs_counts_idx", nil},
		{"terminal", `SELECT id FROM runs WHERE status NOT IN ('queued','running') ORDER BY ` + runEndTimeSQL + ` DESC,id DESC LIMIT 100`, "INDEX runs_terminal_end_idx", nil},
		{"successor", `SELECT 1 FROM runs AS newer WHERE newer.trigger!='promotion' AND newer.repo_id=? AND newer.ref_kind=? AND newer.ref=? AND newer.schedule_entry=? AND newer.queue_sequence>? AND newer.status IN ('queued','running')`, "INDEX runs_active_ref_idx", []any{1, "branch", "ref-0001", "", 1}},
		{"audit", `SELECT id FROM audit_actions WHERE action=? AND resource_type=? ORDER BY id DESC LIMIT 1`, "COVERING INDEX audit_actions_type_idx", []any{"backup.completed", "system"}},
		{"scoped_heads", latestRefQuery(" AND repo_id=?", priorityOrderSQL(s.now())), "SEARCH runs USING COVERING INDEX runs_latest_ref_idx (repo_id=?)", []any{1, "", MaxActiveRuns, "", 0, 0, "", 100}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+check.query, check.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			found := false
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(detail, check.index) {
					found = true
					t.Log(detail)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatalf("query does not use %s", check.index)
			}
		})
	}
}

func BenchmarkAPIReadQueriesLargeHistory(b *testing.B) {
	s := latestRefLargeFixture(b)
	for name, call := range map[string]func() error{
		"counts": func() error { _, err := s.CountRuns(b.Context()); return err },
		"grouped": func() error {
			_, err := s.ListRecentRuns(b.Context(), model.RunListFilter{LatestPerRef: true, Limit: 100})
			return err
		},
		"flat": func() error {
			_, err := s.ListRecentRuns(b.Context(), model.RunListFilter{Limit: 100})
			return err
		},
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := call(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
