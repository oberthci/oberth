package store

import (
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestAtomicServiceFacts(t *testing.T) {
	t.Parallel()
	checkAtomicFacts(t, []atomicFact{
		{"run.cancel", func(t *testing.T, s *Store) func(stateport.Action) error {
			r, _ := accepted(t, s, "cancel")
			return func(a stateport.Action) error {
				_, err := s.Cancel(t.Context(), a, stateport.RunCancellation{ID: r.ID, Revision: r.Revision})
				return err
			}
		}},
	})
}

func TestRetiredGrantAPIsAreAbsent(t *testing.T) {
	t.Parallel()
	if _, ok := reflect.TypeFor[stateport.PublicationIntent]().FieldByName("GrantIDs"); ok {
		t.Fatal("publication carries unused grants")
	}
	for _, name := range []string{"Held", "Holds", "ClearObligation", "SecretAllowed", "DeleteBranch", "GrantPromotion", "RevokePromotionGrant", "PromotionGrants"} {
		if _, ok := reflect.TypeFor[*Store]().MethodByName(name); ok {
			t.Fatal("unused storage method remains", name)
		}
	}
	s := testStore(t)
	var count int
	requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('grants','publication_grants')`).Scan(&count))
	if count != 0 {
		t.Fatal("retired promotion grant schema remains")
	}
}
