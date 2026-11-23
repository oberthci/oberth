package store

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestLatestRunsSeekIndexesBeforeReadingPayloads(t *testing.T) {
	s := testStore(t)
	for _, repository := range []bool{false, true} {
		filter := stateport.RunFilter{Limit: 50, Latest: !repository, LatestRepository: repository}
		query, args := runListQuery(filter, 0)
		rows, err := s.readers.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
		requireOK(t, err)
		var steps []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			requireOK(t, rows.Scan(&id, &parent, &unused, &detail))
			steps = append(steps, detail)
		}
		requireOK(t, rows.Err())
		requireOK(t, rows.Close())
		plan := strings.Join(steps, "\n")
		t.Log(plan)
		index := "run_ref"
		if repository {
			index = "run_history"
		}
		if !strings.Contains(plan, "SEARCH candidate USING COVERING INDEX "+index) || !strings.Contains(plan, "SEARCH runs USING INTEGER PRIMARY KEY") {
			t.Fatal("latest groups must seek their index before loading admission-bearing rows", plan)
		}
	}
}

func TestLatestRunQueryCost(t *testing.T) {
	count, _ := strconv.Atoi(os.Getenv("PERF_LATEST_RUNS"))
	if count == 0 {
		t.Skip("opt-in history measurement")
	}
	s := testStore(t)
	r, _ := accepted(t, s, "seed")
	_, err := s.writer.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS(VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<?)
INSERT INTO runs(id,repo_id,ref,status,release,origin,summary,admission,step_count,steps_planned_exact,steps_pending)
SELECT 'history-'||i,repo_id,'refs/heads/topic-'||(i%40),status,0,origin,
json_set(summary,'$.id','history-'||i,'$.ref','refs/heads/topic-'||(i%40)),zeroblob(12600),0,1,0 FROM runs,n WHERE runs.id=?`, count, r.ID)
	requireOK(t, err)
	for _, repository := range []bool{true, false} {
		filter := stateport.RunFilter{Latest: !repository, LatestRepository: repository, Limit: 50}
		result := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				if _, err := s.Runs(t.Context(), filter); err != nil {
					b.Fatal(err)
				}
			}
		})
		t.Logf("runs=%d admission=12600B latestRepository=%v: %s; %d B/op", count, repository, result.String(), result.AllocedBytesPerOp())
	}
}
