package store

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestEventDedupeContentAndUpperCursor(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	input := atomicEvent(s, "one")
	first, err := eventForTest(t.Context(), s, input)
	requireOK(t, err)
	same, err := eventForTest(t.Context(), s, input)
	requireOK(t, err)
	if same != first {
		t.Fatal("replay changed event id")
	}
	input.Event.Summary = "different"
	_, err = eventForTest(t.Context(), s, input)
	requireCode(t, err, publicerr.Conflict)
	for _, key := range []string{"two", "three"} {
		_, err = eventForTest(t.Context(), s, atomicEvent(s, key))
		requireOK(t, err)
	}
	page, err := s.Events(t.Context(), stateport.EventFilter{After: first, Through: first + 1, Limit: 200})
	requireOK(t, err)
	if len(page.Events) != 1 || page.Events[0].ID != first+1 || page.HasMore {
		t.Fatal("upper cursor escaped", page)
	}
}

func TestAuditAndConfigurationInputRefusals(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	requireCode(t, s.RegisterKey(t.Context(), stateport.Action{}, stateport.Key{Fingerprint: "key", Identity: "operator", Kind: "operator"}), publicerr.InvalidInput)
	_, _, err := s.Audit(t.Context(), -1, 200)
	requireCode(t, err, publicerr.InvalidInput)
	options := testOptions()
	options.Clock = nil
	_, err = Open(t.Context(), s.path, options)
	requireCode(t, err, publicerr.InvalidInput)
	options = testOptions()
	options.BusyTimeout = -1
	_, err = Open(t.Context(), s.path, options)
	requireCode(t, err, publicerr.InvalidInput)
	_, err = Open(t.Context(), t.TempDir(), testOptions())
	requireCode(t, err, publicerr.InvalidInput)
}

func TestPrunedWatermarkAdvancesWithinItsKind(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	first := atomicEvent(s, "one")
	first.Event.At = first.Event.At.Add(-time.Hour)
	_, err := eventForTest(t.Context(), s, first)
	requireOK(t, err)
	requireOK(t, s.RetainEvents(t.Context(), s.options.Clock(), s.options.Clock()))
	_, err = eventForTest(t.Context(), s, atomicEvent(s, "kept"))
	requireOK(t, err)
	late := atomicEvent(s, "late-old-progress")
	late.Event.At = first.Event.At
	last, err := eventForTest(t.Context(), s, late)
	requireOK(t, err)
	requireOK(t, s.RetainEvents(t.Context(), s.options.Clock(), s.options.Clock()))
	page, err := s.Events(t.Context(), stateport.EventFilter{After: last - 1, Limit: 200})
	requireOK(t, err)
	if !page.Gap {
		t.Fatal("pruning watermark stopped advancing")
	}
}
