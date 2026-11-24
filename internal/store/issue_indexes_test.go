package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestIssueTotalsReadIndexWithoutBodies(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	filter := model.IssueFilter{State: "open", Limit: 50, Windows: model.IssueWindows{General: 24 * time.Hour, Active: 2 * time.Hour}}
	predicate, args := scopedIssuePredicate(stateport.IssueScope{Upstreams: []string{"origin"}})
	list := issueList{filter: filter}
	list.set, list.args = issueSet(predicate, args, filter, s.options.Clock())
	query, args := list.totalsQuery(nil)
	s.assertIssueIndex(t, query, args, "issue_totals")
}

func TestCIIssueLookupSeeksRepositoryAndBranch(t *testing.T) {
	t.Parallel()
	testStore(t).assertIssueIndex(t, ciIssueLookup, []any{"origin/team/repo", "refs/heads/topic"}, "issue_ci")
}

// Expression indexes can have a deferred table cursor without reading it.
// Check the actual bytecode: no issue body column may enter either hot read.
func (s *Store) assertIssueIndex(t *testing.T, query string, args []any, name string) {
	t.Helper()
	var tableRoot, indexRoot int
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT rootpage FROM sqlite_master WHERE name='issues'`).Scan(&tableRoot))
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT rootpage FROM sqlite_master WHERE name=?`, name).Scan(&indexRoot))
	rows, err := s.readers.QueryContext(t.Context(), "EXPLAIN "+query, args...)
	requireOK(t, err)
	defer func() { requireOK(t, rows.Close()) }()
	table := -1
	used := false
	for rows.Next() {
		var addr, p1, p2, p3, p5 int
		var op string
		var p4, comment sql.NullString
		requireOK(t, rows.Scan(&addr, &op, &p1, &p2, &p3, &p4, &p5, &comment))
		if op == "OpenRead" && p2 == tableRoot {
			table = p1
		}
		used = used || op == "OpenRead" && p2 == indexRoot
		if op == "Column" && p1 == table {
			t.Fatal("hot issue read loads the issue record", name, p2)
		}
	}
	requireOK(t, rows.Err())
	if !used {
		t.Fatal("issue lookup did not use its index", name)
	}
}

func BenchmarkCIIssueLookup(b *testing.B) {
	s := benchmarkStore(b)
	_, err := s.writer.ExecContext(b.Context(), `WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<4999)
INSERT INTO issues(revision,upstream,repo,state,value)
SELECT 1,'origin','origin/team/repo','open',json_object('kind',CASE i WHEN 0 THEN 'ci' ELSE 'manual' END,'branch','refs/heads/topic','body',?) FROM n`, strings.Repeat("b", 4096))
	if err != nil {
		b.Fatal(err)
	}
	for _, ref := range []string{"refs/heads/topic", "refs/heads/missing"} {
		b.Run(ref, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var id int64
				err := s.readers.QueryRowContext(b.Context(), ciIssueLookup, "origin/team/repo", ref).Scan(&id)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					b.Fatal(err)
				}
			}
		})
	}
}
