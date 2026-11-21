package store

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func queryPlan(t *testing.T, s *Store, query string) string {
	t.Helper()
	rows, err := s.readers.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query)
	requireOK(t, err)
	defer func() { requireOK(t, rows.Close()) }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		requireOK(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	requireOK(t, rows.Err())
	return strings.Join(details, "; ")
}

func TestPollingAndStatusQueriesUseTheirIndexes(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	for _, test := range []struct{ query, index string }{
		{`SELECT id FROM runs WHERE status IN ('queued','running')`, "run_status"},
		{`SELECT sequence FROM runs WHERE repo_id=1 AND status='queued' ORDER BY sequence DESC LIMIT 200`, "run_repo_status"},
		{`SELECT run_id FROM obligations WHERE value<>''`, "obligation_active"},
		{`SELECT run_id FROM publications WHERE status='pending'`, "publication_status"},
		{`SELECT id FROM events WHERE kind='run.updated' ORDER BY id DESC LIMIT 1 OFFSET 9999`, "event_kind_id"},
		{`SELECT coalesce(max(through),0) FROM event_pruned`, "event_pruned_head"},
		{`SELECT id FROM events WHERE kind<>'run.updated' AND at<'2020' LIMIT 512`, "event_ordinary_age"},
	} {
		plan := queryPlan(t, s, test.query)
		t.Log(test.query, plan)
		if !strings.Contains(plan, test.index) {
			t.Fatal("polling predicate lost its index", plan)
		}
	}
}

// The opt-in scale run repeats retention at two million events. Completion,
// rather than host scheduling speed, determines when the next writer runs.
func TestSecondRetentionScale(t *testing.T) {
	t.Parallel()
	count := 15000
	if os.Getenv("OBERTH_C7_SCALE") == "1" {
		count = 2000000
	}
	s := testStore(t)
	seedScaleEvents(t, s, count)
	second, err := Open(t.Context(), s.path, s.options)
	requireOK(t, err)
	defer func() { requireOK(t, second.Close()) }()
	done := make(chan error, 1)
	go func() {
		done <- s.RetainEvents(t.Context(), s.options.Clock().Add(-time.Hour), s.options.Clock().Add(-time.Hour))
	}()
	requireOK(t, <-done)
	_, err = second.CreateIssue(t.Context(), stateport.Action{Actor: "operator"}, model.Issue{Kind: model.IssueManual, Title: "ordinary write after retention"})
	requireOK(t, err)
	var remaining int
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM events`).Scan(&remaining))
	if remaining != 10000 {
		t.Fatal("retention count", remaining)
	}
	t.Logf("ordinary independent writer completed after pruning %d events", count)
}

func TestRetentionBatchReleasesWriter(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	seedScaleEvents(t, s, 1024)
	second, err := Open(t.Context(), s.path, s.options)
	requireOK(t, err)
	defer func() { requireOK(t, second.Close()) }()
	count, err := s.pruneEvents(t.Context(), pruneSelection{predicate: `kind='run.updated' AND id<?`, argument: 1025}, nil)
	requireOK(t, err)
	if count != 512 {
		t.Fatal("retention batch size", count)
	}
	_, err = second.CreateIssue(t.Context(), stateport.Action{Actor: "operator"}, model.Issue{Kind: model.IssueManual, Title: "ordinary write between retention batches"})
	requireOK(t, err)
	var remaining int
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM events`).Scan(&remaining))
	if remaining != 512 {
		t.Fatal("events awaiting the next batch", remaining)
	}
}

func seedScaleEvents(t *testing.T, s *Store, count int) {
	t.Helper()
	_, err := s.writer.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<?)
  INSERT INTO events(repo,kind,dedupe,at,value) SELECT 'origin/team/repo','run.updated','scale-'||i,?, '{}' FROM n`, count, s.options.Clock().Format(timestampFormat))
	requireOK(t, err)
}

func TestAuditPageAtTwoMillionNativeObservations(t *testing.T) {
	t.Parallel()
	if os.Getenv("OBERTH_C7_SCALE") != "1" {
		t.Skip("opt-in two million observation measurement")
	}
	s := testStore(t)
	r, _ := accepted(t, s, "scale")
	head := independentHead(t, s.readers)
	_, err := s.writer.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<2000000)
  INSERT INTO nodes(run_id,node_id,path,credential,value) SELECT ?, 'node-'||i,'build',0,'{}' FROM n`, r.ID)
	requireOK(t, err)
	if independentHead(t, s.readers) != head {
		t.Fatal("observations grew audit")
	}
	plan := queryPlan(t, s, `SELECT * FROM audit WHERE id>0 ORDER BY id LIMIT 200`)
	t.Log("audit seek:", plan)
	if !strings.Contains(plan, "INTEGER PRIMARY KEY") {
		t.Fatal(plan)
	}
	ctx := t.Context()
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if _, _, err := s.Audit(ctx, 0, 200); err != nil {
				b.Fatal(err)
			}
		}
	})
	t.Logf("2,000,000 native rows; audit rows=%d; audit page=%s; bytes/op=%d", head.ID, result.String(), result.AllocedBytesPerOp())
}

func TestStatusQueriesAtOneHundredThousandRuns(t *testing.T) {
	t.Parallel()
	if os.Getenv("OBERTH_C7_SCALE") != "1" {
		t.Skip("opt-in status query measurement")
	}
	s := testStore(t)
	r, _ := accepted(t, s, "seed")
	_, err := s.writer.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS(VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<100000)
  INSERT INTO runs(id,repo_id,ref,status,release,origin,summary,admission,step_count,steps_planned_exact,steps_pending)
  SELECT 'scale-'||i,repo_id,ref,'passed',0,origin,json_set(summary,'$.id','scale-'||i,'$.status','passed'),admission,0,1,0 FROM runs,n WHERE runs.id=?`, r.ID)
	requireOK(t, err)
	for _, query := range []struct {
		name string
		call func() error
	}{
		{"status", func() error {
			_, err := s.Runs(t.Context(), stateport.RunFilter{Repositories: []string{r.Repo, "origin/team/other"}, Status: stepmodel.RunQueued, Limit: 200})
			return err
		}},
		{"idle", func() error { _, err := s.Idle(t.Context()); return err }},
	} {
		result := testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				if err := query.call(); err != nil {
					b.Fatal(err)
				}
			}
		})
		t.Log(fmt.Sprint(query.name, ": ", result.String()))
	}
}
