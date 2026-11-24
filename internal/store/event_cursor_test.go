package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestOwnEventCursorSeeksNewEvents(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	for _, scope := range [][]string{nil, {"origin/team/repo"}, {"origin/team/repo", "origin/team/other"}} {
		query, args := eventQuery(stateport.EventFilter{Actor: "alice", Repositories: scope, After: 100, Limit: 50}, 101)
		rows, err := s.readers.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
		requireOK(t, err)
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			requireOK(t, rows.Scan(&id, &parent, &unused, &detail))
			plan.WriteString(detail + "\n")
		}
		requireOK(t, rows.Err())
		requireOK(t, rows.Close())
		text := plan.String()
		if !strings.Contains(text, "SEARCH e") || !strings.Contains(text, ">?") || !strings.Contains(text, "SEARCH r") || strings.Contains(text, "run_actor") {
			t.Fatal("cursor wake starts from run history", text)
		}
		t.Log(text)
	}
}

func benchmarkStore(b *testing.B) *Store {
	b.Helper()
	s, err := Create(b.Context(), filepath.Join(b.TempDir(), "state.sqlite"), testOptions())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

// A caller owns one tenth of 20,000 runs; one event arrives after its cursor.
func BenchmarkOwnEventCursor(b *testing.B) {
	s := benchmarkStore(b)
	_, err := s.writer.ExecContext(b.Context(), `INSERT INTO upstreams(name,org,kind,base_url,key_name) VALUES('origin','team','git','https://oberth.ci','source');
INSERT INTO repositories(id,upstream,name,canonical) VALUES(1,'origin','team/repo','origin/team/repo')`)
	if err != nil {
		b.Fatal(err)
	}
	_, err = s.writer.ExecContext(b.Context(), `WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<19999)
INSERT INTO runs(id,repo_id,ref,status,release,origin,summary,admission,step_count,steps_planned_exact,steps_pending)
SELECT printf('history-%d',i),1,'refs/heads/main','queued',0,json_object('actor',CASE i%10 WHEN 0 THEN 'alice' ELSE 'bob' END),'{}',?,1,1,1 FROM n`, []byte(`{"padding":"`+strings.Repeat("b", 12600)+`"}`))
	if err != nil {
		b.Fatal(err)
	}
	event := encode(model.AgentEvent{Kind: "run.updated", Repo: "origin/team/repo", RunID: "history-0", At: s.options.Clock()})
	_, err = s.writer.ExecContext(b.Context(), `WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<19999)
INSERT INTO events(repo,kind,dedupe,at,value) SELECT 'origin/team/repo','run.updated',printf('old-%d',i),'',json_set(?,'$.run_id',printf('history-%d',i)) FROM n;
INSERT INTO events(repo,kind,dedupe,at,value) VALUES('origin/team/repo','run.updated','new','',?)`, event, event)
	if err != nil {
		b.Fatal(err)
	}
	q := stateport.EventFilter{Actor: "alice", After: 20000, Limit: 500}
	b.ReportAllocs()
	for b.Loop() {
		page, err := s.Events(b.Context(), q)
		if err != nil || len(page.Events) != 1 {
			b.Fatal(err, page)
		}
	}
}
