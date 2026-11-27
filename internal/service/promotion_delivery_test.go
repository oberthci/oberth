package service_test

import (
	"context"
	"reflect"
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

type promotionReadHook struct {
	service.PromotionStore
	read func(context.Context, string) (model.Promotion, error)
}

func (s promotionReadHook) Promotion(ctx context.Context, id string) (model.Promotion, error) {
	return s.read(ctx, id)
}

func TestPromotionReauthorizesAfterFinalSnapshot(t *testing.T) {
	for _, path := range []string{"timeout", "settled"} {
		for _, change := range []string{"revoke", "hide-repository", "unchanged"} {
			t.Run(path+"/"+change, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) { checkPromotionDelivery(t, path, change) })
			})
		}
	}
}

func checkPromotionDelivery(t *testing.T, path, change string) {
	t.Helper()
	f := setup(t)
	f.seed("running")
	status, finalRead := model.PromotionPending, 3
	if path == "settled" {
		status, finalRead = model.PromotionFailed, 2
	}
	p := model.Promotion{ID: "pending", Repo: repository, ResultSHA: head, Actor: caller().Identity, Caller: caller(), Status: status}
	if err := f.store.PreparePromotion(t.Context(), stateport.Action{Actor: caller().Identity}, p); err != nil {
		t.Fatal(err)
	}
	if path == "settled" {
		if err := f.store.FinishPromotion(t.Context(), stateport.Action{Actor: caller().Identity}, stateport.PromotionUpdate{ID: p.ID, Revision: 1, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	reads := 0
	f.c.Promotions = promotionReadHook{PromotionStore: f.store, read: func(ctx context.Context, id string) (model.Promotion, error) {
		p, err := f.store.Promotion(ctx, id)
		reads++
		if reads == finalRead && change != "unchanged" {
			changeDeliveryAuthority(t, f, ctx, change)
		}
		return p, err
	}}
	f.restart(t)
	call, err := f.s.PreparePromoteStatus(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteStatusArgs{ID: p.ID, TimeoutS: 1})
	if err != nil {
		t.Fatal(err)
	}
	got, err := call(t.Context())
	if reads != finalRead {
		t.Fatalf("expected %d promotion reads, got %d", finalRead, reads)
	}
	if change != "unchanged" {
		assertPromotionRefused(t, got, err)
		return
	}
	assertPromotionDelivered(t, got, err, p)
}

func assertPromotionRefused(t *testing.T, got apicontract.PromotionWaitResult, err error) {
	t.Helper()
	if err == nil || !reflect.DeepEqual(got, apicontract.PromotionWaitResult{}) {
		t.Fatalf("authority change delivered promotion snapshot: %+v, err=%v", got, err)
	}
}

func assertPromotionDelivered(t *testing.T, got apicontract.PromotionWaitResult, err error, expected model.Promotion) {
	t.Helper()
	if err != nil || got.Promotion == nil || got.Promotion.ID != expected.ID || got.TimedOut != (expected.Status == model.PromotionPending) {
		t.Fatalf("authorized promotion reply: %+v, err=%v", got, err)
	}
}

func changeDeliveryAuthority(t *testing.T, f *fixture, ctx context.Context, change string) {
	t.Helper()
	f.store.mu.Lock()
	a := f.store.Actors[caller().Fingerprint]
	if change == "revoke" {
		delete(f.store.Actors, a.Fingerprint)
	} else {
		a.Scope = []string{"private"}
		f.store.Actors[a.Fingerprint] = a
	}
	f.store.mu.Unlock()
	if err := f.store.native.RecordAction(ctx, model.AuditAction{Actor: a.Identity, Action: "scope.changed", ResourceType: "key", ResourceID: a.Fingerprint}); err != nil {
		t.Fatal(err)
	}
}
