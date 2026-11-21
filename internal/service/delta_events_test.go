package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"strings"
	"testing"
	"testing/synctest"
)

func TestEventsOmittedCursorStartsAtPreparation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		f.seed("running")
		f.store.native.SeedEvent(event(0))
		call, err := f.s.PrepareEventsWait(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.EventsWaitArgs{Own: true, TimeoutS: 1})
		if err != nil {
			t.Fatal(err)
		}
		result, err := call(t.Context())
		if err != nil || !result.TimedOut || len(result.Events) != 0 || result.NextAfterID != 1 {
			t.Fatal("omitted cursor replayed history", result, err)
		}
		call, err = f.s.PrepareEventsWait(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.EventsWaitArgs{TimeoutS: 1})
		if err != nil {
			t.Fatal(err)
		}
		f.store.native.SeedEvent(event(1))
		result, err = call(t.Context())
		if err != nil || result.TimedOut || len(result.Events) != 1 || result.Events[0].ID != 2 {
			t.Fatal("prepare-to-call event was lost", result, err)
		}
	})
}
func TestEventsExplicitZeroReadsRetainedHistory(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		f.store.native.SeedEvent(event(0))
		call, err := f.s.PrepareEventsWait(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.EventsWaitArgs{AfterID: new(int64(0)), TimeoutS: 1})
		if err != nil {
			t.Fatal(err)
		}
		result, err := call(t.Context())
		if err != nil || len(result.Events) != 1 || result.Gap {
			t.Fatal("explicit zero stopped reading retained history", result, err)
		}

	})
}
func TestEventsPreparationPropagatesBoundsFailure(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.c.Events = eventHook{EventStore: f.store, read: func(context.Context, stateport.EventFilter) (model.EventPage, error) {
		return model.EventPage{}, context.DeadlineExceeded
	}}
	f.restart(t)
	call, err := f.s.PrepareEventsWait(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.EventsWaitArgs{})
	if !errors.Is(err, context.DeadlineExceeded) || call != nil {
		t.Fatal("bounds failure created a call", err)
	}
}
func TestDeletionUsesPublicationFailureEventAndStableDedupe(t *testing.T) {
	t.Parallel()
	f := setup(t)
	d := deletion()
	b := stateport.BatchAcceptance{ReceiveID: d.Request.ReceiveID, Repository: repository, Actor: caller(), Deletions: []stateport.BranchDeletion{d}}
	for range 2 {
		if _, err := f.s.Accept(t.Context(), b); err != nil {
			t.Fatal(err)
		}
	}
	before, err := f.store.Events(t.Context(), stateport.EventFilter{Limit: 10})
	if err != nil || len(before.Events) != 0 {
		t.Fatal("deletion created a separate event", before, err)
	}
	failure := strings.Repeat("deletion refused ", 2000)
	f.moving.Push = func(context.Context, model.Publication) (gitport.PublicationReceipt, error) {
		return gitport.PublicationReceipt{}, errors.New(failure)
	}
	for range 2 {
		if err = f.s.Deliver(t.Context(), d.Publication.ID); err != nil {
			t.Fatal(err)
		}
	}
	page, err := f.store.Events(t.Context(), stateport.EventFilter{Limit: 10})
	if err != nil || len(page.Events) != 1 || page.Events[0].Kind != string(model.EventPublicationFailed) {
		t.Fatal("deletion failure did not use publication event", page, err)
	}
	assertDeletionPublicationFact(t, page.Events[0], d.Publication)
	stored, err := f.store.Publication(t.Context(), d.Publication.ID)
	if err != nil || stored.Detail != failure {
		t.Fatal("publication failure detail lost", stored, err)
	}
}

func assertDeletionPublicationFact(t *testing.T, event model.AgentEvent, want model.Publication) {
	t.Helper()
	var fact model.Publication
	if err := json.Unmarshal(event.Data, &fact); err != nil {
		t.Fatal(err)
	}
	if event.RunID != "" || fact.ID != want.ID || fact.Repo != want.Repo || fact.Ref != want.Ref || fact.ObjectSHA != "" {
		t.Fatal("deletion target missing", fact)
	}
	if fact.PreviousSHA == nil || *fact.PreviousSHA != *want.PreviousSHA || fact.Status != model.PublicationFailed || fact.Detail != "" {
		t.Fatal("deletion predecessor/state missing or unbounded", fact)
	}
}
