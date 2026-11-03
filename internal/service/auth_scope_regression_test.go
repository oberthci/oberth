package service

import (
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/store"
)

func TestScopedAdminIssueMoveRequiresVisibleTarget(t *testing.T) {
	for _, target := range []string{"codeberg", "", "global"} {
		t.Run(target, func(t *testing.T) {
			fixture := newScopeFixture(t)
			before, err := fixture.store.Issue(t.Context(), fixture.global.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = fixture.call(scopedAdmin, "issue_update", map[string]any{"id": before.ID, "upstream": target})
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("move to %q: %v, want not found", target, err)
			}
			after, err := fixture.store.Issue(t.Context(), before.ID)
			if err != nil || after.ScopeUpstreamID != before.ScopeUpstreamID {
				t.Fatalf("refused move changed issue: %+v, %v", after, err)
			}
		})
	}
}
