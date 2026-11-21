package store

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestResolveRunUsesIndexedLiteralPrefixes(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	for _, id := range []string{"run", "run-later", "wild%_one", "wildZZone", "é-run", "\U0010ffff-root"} {
		_, _ = accepted(t, s, id)
	}
	for selector, want := range map[string]string{"run": "run", "wild%_": "wild%_one", "é": "é-run", "\U0010ffff": "\U0010ffff-root"} {
		got, err := s.ResolveRun(t.Context(), nil, selector)
		requireOK(t, err)
		if got.ID != want {
			t.Fatal("prefix changed its literal meaning", selector, got.ID)
		}
	}
	_, err := s.ResolveRun(t.Context(), nil, "wild")
	requireCode(t, err, publicerr.Ambiguous)
	for _, scope := range [][]string{nil, {"origin/team/repo"}, {"origin/team/repo", "origin/team/other"}} {
		checkPrefixIndex(t, s, scope)
	}
}

func checkPrefixIndex(t *testing.T, s *Store, scope []string) {
	t.Helper()
	query, args := resolveQuery(scope, "run")
	rows, err := s.readers.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	requireOK(t, err)
	defer func() { requireOK(t, rows.Close()) }()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		requireOK(t, rows.Scan(&id, &parent, &unused, &detail))
		plan.WriteString(detail)
	}
	requireOK(t, rows.Err())
	if !strings.Contains(plan.String(), "id>? AND id<?") || strings.Contains(plan.String(), "SCAN runs") || strings.Contains(plan.String(), "TEMP B-TREE") {
		t.Fatal("prefix lookup scans run history", plan.String())
	}
	t.Log(plan.String())
}
