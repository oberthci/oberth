package store

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestAtomicRetentionFacts(t *testing.T) {
	t.Parallel()
	checkAtomicFacts(t, []atomicFact{
		{"events.retain", func(t *testing.T, s *Store) func(stateport.Action) error {
			e := atomicEvent(s, "old")
			e.Event.At = e.Event.At.Add(-2 * time.Hour)
			_, err := eventForTest(t.Context(), s, e)
			requireOK(t, err)
			return func(a stateport.Action) error {
				return s.RetainEvents(t.Context(), s.options.Clock().Add(-time.Hour), s.options.Clock().Add(-time.Hour), a.Events...)
			}
		}},
	})
}
