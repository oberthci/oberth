package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRefGlobUsesBoundedCoveringIndex(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	for pattern, want := range map[string]string{"land/*": "land/", "*": "", `feature/\?x*`: "feature/?x", "land/[ab]": "land/", "é/*": "é/"} {
		if got := refGlobPrefix(pattern); got != want {
			t.Fatalf("%q prefix=%q want %q", pattern, got, want)
		}
	}
	restriction, args := runRestriction(model.RunListFilter{RefGlob: "land/*"})
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN SELECT id FROM runs WHERE 1=1"+restriction, args...)
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
		found = found || strings.Contains(detail, "SEARCH runs USING COVERING INDEX runs_ref_prefix_idx (ref>? AND ref<?)")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("ref glob has no indexed prefix range search")
	}
}
