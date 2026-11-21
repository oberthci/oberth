package contracttest

import (
	"testing"

	"context"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

type PublicationPort interface {
	PublishedRelease(context.Context, string, string) (model.Publication, error)
	PreparePublication(context.Context, stateport.Action, stateport.PublicationIntent) error
	Publication(context.Context, string) (model.Publication, error)
	BindPublication(context.Context, stateport.Action, string, string) (model.Publication, error)
	StartPublicationAttempt(context.Context, stateport.Action, stateport.PublicationAttempt) (model.Publication, error)
	SettlePublication(context.Context, stateport.Action, stateport.PublicationResult) error
	RetryPublication(context.Context, stateport.Action, stateport.PublicationAttempt) error
	AbandonPublication(context.Context, stateport.Action, string) error
}
type PromotionPort interface {
	PreparePromotion(context.Context, stateport.Action, model.Promotion) error
	Promotion(context.Context, string) (model.Promotion, error)
	PromotionForRun(context.Context, string) (model.Promotion, error)
	FinishPromotion(context.Context, stateport.Action, stateport.PromotionUpdate) error
}
type HoldPort interface {
	Assist(context.Context, stateport.Action, stateport.HoldAction) error
}
type MovingPort interface {
	RunPort
	PublicationPort
	PromotionPort
	HoldPort
}

// PublicationStore specifies immutable base binding, attempt CAS and settlement.
// The fixture's passing run is already present; the publication has an unknown base.
func PublicationStore(t *testing.T, p PublicationPort, publication model.Publication) {
	t.Helper()
	ctx := t.Context()
	a := stateport.Action{Actor: publication.Actor, Reason: "publication contract"}
	publication.PreviousSHA = nil
	if err := p.PreparePublication(ctx, a, stateport.PublicationIntent{Publication: publication}); err != nil {
		t.Fatal(err)
	}
	bound, err := p.BindPublication(ctx, a, publication.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if bound.PreviousSHA == nil || *bound.PreviousSHA != "" {
		t.Fatal("absent predecessor lost")
	}
	if _, err := p.BindPublication(ctx, a, publication.ID, "different"); err == nil {
		t.Fatal("bound predecessor changed")
	}
	started, err := p.StartPublicationAttempt(ctx, a, stateport.PublicationAttempt{ID: publication.ID})
	if err != nil {
		t.Fatal(err)
	}
	if started.Attempts != 1 {
		t.Fatal("claim did not advance")
	}
	if _, err := p.StartPublicationAttempt(ctx, a, stateport.PublicationAttempt{ID: publication.ID}); err == nil {
		t.Fatal("stale claim accepted")
	}
	finishPublicationContract(t, p, publication)
}
func finishPublicationContract(t *testing.T, p PublicationPort, publication model.Publication) {
	t.Helper()
	ctx := t.Context()
	id := publication.ID
	a := stateport.Action{Actor: publication.Actor, Reason: "publication contract"}
	if err := p.SettlePublication(ctx, a, stateport.PublicationResult{ID: id, Attempt: 2, Delivered: true}); err == nil {
		t.Fatal("stale settlement accepted")
	}
	if err := p.SettlePublication(ctx, a, stateport.PublicationResult{ID: id, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := p.RetryPublication(ctx, a, stateport.PublicationAttempt{ID: id, ExpectedAttempts: 2}); err == nil {
		t.Fatal("stale retry budget accepted")
	}
	if err := p.RetryPublication(ctx, a, stateport.PublicationAttempt{ID: id, ExpectedAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.StartPublicationAttempt(ctx, a, stateport.PublicationAttempt{ID: id, ExpectedAttempts: 1}); err == nil {
		t.Fatal("stale claim survived explicit budget reset")
	}
	if err := p.AbandonPublication(ctx, a, id); err != nil {
		t.Fatal(err)
	}
	result, err := p.Publication(ctx, id)
	if err != nil || result.Status != model.PublicationAbandoned || result.Attempts != 0 {
		t.Fatal("reset/abandon lost", result, err)
	}
}

// PromotionStore specifies detached private attribution and immutable revision fences.
func PromotionStore(t *testing.T, p PromotionPort, input model.Promotion) {
	t.Helper()
	ctx := t.Context()
	a := stateport.Action{Actor: input.Actor, Reason: "promotion contract"}
	if err := p.PreparePromotion(ctx, a, input); err != nil {
		t.Fatal(err)
	}
	row, err := p.Promotion(ctx, input.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.Caller.Scope[0] = "changed"
	row, err = p.Promotion(ctx, input.ID)
	if err != nil || row.Caller.Scope[0] != input.Caller.Scope[0] {
		t.Fatal("private caller aliases stored scope")
	}
	q := stateport.PromotionUpdate{ID: row.ID, Revision: row.Revision, Status: model.PromotionFailed, Reason: "promotion failed"}
	if err := p.FinishPromotion(ctx, a, q); err != nil {
		t.Fatal(err)
	}
	if err := p.FinishPromotion(ctx, a, q); err == nil {
		t.Fatal("stale promotion verdict accepted")
	}
	row, err = p.Promotion(ctx, input.ID)
	if err != nil || row.Status != model.PromotionFailed {
		t.Fatal("promotion verdict missing", row, err)
	}
}
