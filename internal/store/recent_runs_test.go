package store

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

// TestRecentRunsWalksTheHistoryIndex pins the cost of the repository strip:
// the named repositories are looked up by name, each one's newest runs come
// from a bounded walk of run_history, and each run row by its key, so no plan
// step scans the run history.
func TestRecentRunsWalksTheHistoryIndex(t *testing.T) {
	s := testStore(t)
	predicate, args := scopePredicate("repositories.canonical", []string{"origin/team/repo", "origin/team/other"})
	rows, err := s.readers.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+recentRunsQuery+predicate+` ORDER BY repositories.id,runs.sequence`,
		append([]any{stateport.RecentRunLimit}, args...)...)
	requireOK(t, err)
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		requireOK(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	requireOK(t, rows.Err())
	text := strings.Join(plan, "\n")
	for _, step := range plan {
		if strings.HasPrefix(step, "SCAN runs") || strings.HasPrefix(step, "SCAN recent") {
			t.Fatal("the strip scans run history:\n" + text)
		}
	}
	if !strings.Contains(text, "USING INDEX run_history") && !strings.Contains(text, "USING COVERING INDEX run_history") {
		t.Fatal("the strip does not walk run_history:\n" + text)
	}
}
