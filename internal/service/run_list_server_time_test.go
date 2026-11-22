package service_test

import "testing"

// A run page carries the service's clock, so a reader ages the runs against the clock
// that stamped them instead of its own.
func TestRunListCarriesServerTime(t *testing.T) {
	t.Parallel()
	p := upstreamRuns(t)
	page, err := p.f.s.RunList(t.Context(), admin(), viewQuery{})
	if err != nil || len(page.Runs) == 0 || !page.ServerTime.Equal(at()) {
		t.Fatal("run list without the server's clock", page.ServerTime, err)
	}
	f := heldFixture(t)
	held, err := f.s.RunList(t.Context(), caller(), viewQuery{Held: true, Limit: 50})
	if err != nil || len(held.Runs) == 0 || !held.ServerTime.Equal(at()) {
		t.Fatal("held run list without the server's clock", held.ServerTime, err)
	}
}
