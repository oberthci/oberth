package app_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/app"
)

func TestDurableReceiveAcknowledgementDoesNotNeedNewAuthorityOrSource(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	if _, err := s.Deliver(t.Context(), app.Receive{ID: "not-recorded"}); err == nil {
		t.Fatal("new receive without updates was accepted")
	}
	r := f.gate(t, s)
	if _, err := s.Deliver(t.Context(), app.Receive{ID: r.ID}); err == nil {
		t.Fatal("a quarantined gate was accepted without the receive event")
	}
	first, err := s.Deliver(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	prepares, creates := f.prepares.Load(), f.creates.Load()
	next, err := s.Deliver(t.Context(), app.Receive{ID: r.ID})
	if err != nil || len(next.Outcomes) != len(first.Outcomes) || next.Outcomes[0].RunID != first.Outcomes[0].RunID {
		t.Fatal("durable receive cannot be acknowledged", next, err)
	}
	if f.prepares.Load() != prepares || f.creates.Load() != creates {
		t.Fatal("acknowledging a durable receive repeated work")
	}
}
