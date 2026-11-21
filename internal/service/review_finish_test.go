package service_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"sync/atomic"
	"testing"
)

type unreadablePromotion struct {
	service.PromotionStore
	err error
}

func (p unreadablePromotion) PromotionForRun(context.Context, string) (model.Promotion, error) {
	return model.Promotion{}, p.err
}
func TestPromotionLookupFailureAlwaysFinishesAndLogsOnce(t *testing.T) {
	t.Parallel()
	f := setup(t)
	state := landedStore(t, f)
	landedRepository(t, state)
	f.c.Runs, f.c.Catalog, f.c.Events, f.c.Publications = state, state, state, state
	failure := errors.New("promotion database unavailable")
	f.c.Promotions = unreadablePromotion{PromotionStore: state, err: failure}
	f.restart(t)
	b := batch(t)
	b.Runs[0].Request.Trigger = model.TriggerPromotion
	b.Runs[0].Prepared = landedAdmission(t, runID, b.Runs[0].Request)
	accepted, e := f.s.Accept(t.Context(), b)
	if e != nil {
		t.Fatal(e)
	}
	id := accepted.Runs[0].ID
	for range 2 {
		if e = f.s.Finished(t.Context(), service.FinishedRun{RunID: id, Status: stepmodel.RunPassed}); e != nil {
			t.Fatal("promotion stuck", e)
		}
	}
	assertPromotionReadFailure(t, f, id, failure)
	assertUnrelatedRunFinishes(t, f)
}
func assertUnrelatedRunFinishes(t *testing.T, f *fixture) {
	t.Helper()
	b := batch(t)
	b.ReceiveID = "unrelated"
	b.Runs[0].Request.ReceiveID = b.ReceiveID
	b.Runs[0].Prepared = landedAdmission(t, fmt.Sprintf("%032x", 99), b.Runs[0].Request)
	accepted, e := f.s.Accept(t.Context(), b)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.s.Finished(t.Context(), service.FinishedRun{RunID: accepted.Runs[0].ID, Status: stepmodel.RunFailed}); e != nil {
		t.Fatal("unrelated run blocked", e)
	}
}
func TestConcurrentReleaseRerunsPublishAndAnnounceOnce(t *testing.T) {
	t.Parallel()
	f := setup(t)
	state := landedStore(t, f)
	landedRepository(t, state)
	f.c.Runs, f.c.Catalog, f.c.Events, f.c.Publications = state, state, state, state
	f.restart(t)
	ids := acceptReleasePair(t, f)
	var calls atomic.Int64
	publish := f.moving.Push
	f.moving.Push = func(ctx context.Context, p model.Publication) (gitport.PublicationReceipt, error) {
		calls.Add(1)
		return publish(ctx, p)
	}
	done := make(chan error, 2)
	start := make(chan struct{})
	for _, id := range ids {
		go func() { <-start; done <- f.s.Deliver(t.Context(), id) }()
	}
	close(start)
	for range 2 {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
	receipts := 0
	for _, id := range ids {
		p, e := state.Publication(t.Context(), id)
		if e != nil || p.Status != model.PublicationDelivered {
			t.Fatal(p, e)
		}
		if p.Release != nil {
			receipts++
		}
	}
	page, e := state.Events(t.Context(), stateport.EventFilter{Limit: 200})
	if e != nil || calls.Load() != 1 || receipts != 1 || countEventKind(page, model.EventReleasePublished) != 1 {
		t.Fatal("release repeated", calls.Load(), receipts, page, e)
	}
}

func acceptReleasePair(t *testing.T, f *fixture) []string {
	t.Helper()
	ids := []string{}
	for n := 1; n <= 2; n++ {
		b := batch(t)
		b.ReceiveID = fmt.Sprint("release-", n)
		a := &b.Runs[0]
		a.Request.ReceiveID = b.ReceiveID
		a.Request.Ref = "refs/tags/v1.2.3"
		a.Request.Trigger = model.TriggerTag
		a.Release = true
		a.Prepared = landedAdmission(t, fmt.Sprintf("%032x", n), a.Request)
		accepted, e := f.s.Accept(t.Context(), b)
		if e != nil {
			t.Fatal(e)
		}
		id := accepted.Runs[0].ID
		ids = append(ids, "publication:"+id)
		if e = f.s.Finished(t.Context(), service.FinishedRun{RunID: id, Status: stepmodel.RunPassed}); e != nil {
			t.Fatal(e)
		}
	}

	return ids
}

func assertPromotionReadFailure(t *testing.T, f *fixture, id string, failure error) {
	t.Helper()
	got, e := f.c.Runs.Run(t.Context(), id)
	if e != nil || got.Status != stepmodel.RunFailed || got.DisplayReason != "promotion record unavailable" || got.Detail != failure.Error() {
		t.Fatal("promotion not safely finalized", got, e)
	}
	if len(f.errors) != 1 || !errors.Is(<-f.errors, failure) {
		t.Fatal("diagnostic not logged exactly once")
	}
	if _, e = f.c.Publications.Publication(t.Context(), "publication:"+id); publicerr.Classify(e).Code() != publicerr.NotFound {
		t.Fatal("failed promotion published", e)
	}
	page, e := f.c.Events.Events(t.Context(), stateport.EventFilter{Limit: 200})
	if e != nil || countEventKind(page, model.EventRunFinished) != 1 {
		t.Fatal("terminal event repeated", page, e)
	}
}
