package store

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestReleaseBuildPagesSeekCommitIndexInLargeHistory(t *testing.T) {
	s := testStore(t)
	r, _ := accepted(t, s, "build-seed")
	_, err := s.writer.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS(VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<50000)
INSERT INTO runs(id,repo_id,ref,status,release,origin,summary,admission,step_count,steps_planned_exact,steps_pending)
SELECT 'history-'||i,repo_id,ref,status,release,origin,json_set(summary,'$.id','history-'||i),admission,step_count,steps_planned_exact,steps_pending
FROM runs,n WHERE runs.id=?`, r.ID)
	requireOK(t, err)
	filter := stateport.RunFilter{Repositories: []string{r.Repo}, CommitSHA: *r.CommitSHA, Limit: stateport.RunPageLimit}
	probe := probeQueries(t, s)
	for range 2 {
		query, args := runListQuery(filter, filter.Before)
		rows, err := s.readers.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
		requireOK(t, err)
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			requireOK(t, rows.Scan(&id, &parent, &unused, &detail))
			plan = append(plan, detail)
		}
		requireOK(t, rows.Err())
		requireOK(t, rows.Close())
		if !strings.Contains(strings.Join(plan, "\n"), "SEARCH runs USING INDEX run_commit") {
			t.Fatal("release build lookup must seek the exact commit index", plan)
		}
		page, err := s.Runs(t.Context(), filter)
		requireOK(t, err)
		want := int64(50001)
		if filter.Before > 0 {
			want = filter.Before - 1
		}
		if len(page) != stateport.RunPageLimit || page[0].Sequence != want {
			t.Fatal("release build page lost its bound or descending cursor", len(page), filter.Before)
		}
		filter.Before = page[len(page)-1].Sequence
	}
	reads := 0
	for _, query := range probe.take() {
		if !strings.HasPrefix(query, "EXPLAIN ") {
			reads++
		}
	}
	if reads != 2 {
		t.Fatal("two release build pages must use exactly two database reads", reads)
	}
	t.Logf("history=50001 indexed pages=2 rows=400 database reads=%d", reads)
}
