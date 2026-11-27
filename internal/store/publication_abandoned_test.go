package store

import (
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestAbandonedPublicationCannotBeRetried(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	p := publicationFixture(t, s)
	a := stateport.Action{Actor: "operator", Reason: "intent withdrawn"}
	requireOK(t, s.AbandonPublication(t.Context(), a, p.ID))
	before, err := s.Publication(t.Context(), p.ID)
	requireOK(t, err)
	if before.Status != model.PublicationAbandoned || before.Reason != a.Reason {
		t.Fatal("publication was not abandoned", before)
	}
	_, head, err := s.Audit(t.Context(), 0, 100)
	requireOK(t, err)
	changed := observeChanges(t, s)
	err = s.RetryPublication(t.Context(), a, stateport.PublicationAttempt{ID: p.ID, ExpectedAttempts: before.Attempts})
	want := "invalid_transition: requested state transition is invalid - Read the current status and choose an allowed transition."
	if err == nil || err.Error() != want {
		t.Fatalf("retry abandoned publication: %v; want %s", err, want)
	}
	after, err := s.Publication(t.Context(), p.ID)
	requireOK(t, err)
	_, afterHead, err := s.Audit(t.Context(), 0, 100)
	requireOK(t, err)
	if !reflect.DeepEqual(after, before) || afterHead != head {
		t.Fatal("refused retry changed publication or audit", after, head, afterHead)
	}
	pending, err := s.PendingPublications(t.Context(), stateport.RecordFilter{Repositories: []string{p.Repo}, Limit: 100})
	requireOK(t, err)
	if len(pending) != 0 {
		t.Fatal("abandoned publication became pending", pending)
	}
	select {
	case <-changed:
		t.Fatal("refused retry notified a change")
	default:
	}
}
