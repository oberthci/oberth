package service_test

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
	"time"
)

func TestReviewUnrequestedRunNeverListsHolds(t *testing.T) {
	t.Parallel()
	f := heldFixture(t)
	r, err := f.store.Run(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	r.Origin.AssistRequested = false
	f.store.native.SeedRun(r)
	calls := 0
	f.holds.Listing = func(context.Context, stateport.Run) ([]model.LiveHold, error) {
		calls++
		return []model.LiveHold{fixtureHold(runID)}, nil
	}
	call, err := f.s.PrepareRunHoldList(t.Context(), workerCaller(), apicontract.RunHoldListArgs{ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	out, err := call(t.Context())
	if err != nil || len(out.Holds) != 0 || calls != 0 {
		t.Fatal(out, err, calls)
	}
}
func TestReviewHeldListWindowAndPerRunFailure(t *testing.T) {
	t.Parallel()
	f := heldFixture(t)
	r, err := f.store.Run(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	old := r.Clone()
	old.ID = "old-run"
	old.Status = stepmodel.RunFailed
	ended := at().Add(-5 * time.Hour)
	old.FinishedAt = &ended
	f.store.native.SeedRun(old)
	unreadable := r.Clone()
	unreadable.ID = "unreadable-run"
	f.store.native.SeedRun(unreadable)
	seen := map[string]bool{}
	f.holds.Listing = func(_ context.Context, row stateport.Run) ([]model.LiveHold, error) {
		seen[row.ID] = true
		if row.ID == unreadable.ID {
			return nil, errors.New("native unavailable")
		}
		return []model.LiveHold{fixtureHold(row.ID)}, nil
	}
	out, err := f.s.RunList(t.Context(), caller(), viewQuery{Held: true, Limit: 50})
	if err != nil || len(out.Runs) != 1 || out.Runs[0].ID != runID || seen[old.ID] || !seen[unreadable.ID] {
		t.Fatal(out, err, seen)
	}
}
func TestReviewHeldListWindowBoundary(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		status stepmodel.RunStatus
		age    time.Duration
		want   int
	}{{stepmodel.RunFailed, 4*time.Hour + time.Minute, 1}, {stepmodel.RunFailed, 4*time.Hour + time.Minute + time.Nanosecond, 0}, {stepmodel.RunRunning, 5 * time.Hour, 1}} {
		f := heldFixture(t)
		r, err := f.store.Run(t.Context(), runID)
		if err != nil {
			t.Fatal(err)
		}
		r.Status = test.status
		end := at().Add(-test.age)
		r.FinishedAt = &end
		f.store.native.SeedRun(r)
		out, err := f.s.RunList(t.Context(), caller(), viewQuery{Held: true, Limit: 50})
		if err != nil || len(out.Runs) != test.want {
			t.Fatal(test, out, err)
		}
	}
}
