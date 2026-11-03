package service

import (
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestScopedPromotionGrantMatchingRefusesHiddenRepository(t *testing.T) {
	fixture := newScopeFixture(t)
	scoped, err := fixture.service.forActor(t.Context(), scopedAdmin)
	if err != nil {
		t.Fatal(err)
	}
	visible := fixture.grants["github"]
	if grant, err := scoped.promotionGrants.MatchingPromotionGrant(t.Context(), visible.RepoID, visible.SourceSHA, visible.TargetRef); err != nil || grant.ID != visible.ID {
		t.Fatalf("visible grant refused: %+v, %v", grant, err)
	}
	hidden := fixture.grants["codeberg"]
	grant, err := scoped.promotionGrants.MatchingPromotionGrant(t.Context(), hidden.RepoID, hidden.SourceSHA, hidden.TargetRef)
	if !errors.Is(err, store.ErrNotFound) || grant.ID != "" {
		t.Fatalf("hidden grant authorized: %+v, %v", grant, err)
	}
}

func TestScopedPromotionAppendRefusesHiddenRepository(t *testing.T) {
	fixture := newScopeFixture(t)
	scoped, err := fixture.service.forActor(t.Context(), scopedAdmin)
	if err != nil {
		t.Fatal(err)
	}
	spec := model.PromotionSpec{RepoID: fixture.visible.ID, SourceBranch: "feature/guard", SourceSHA: sha("e"), TargetRef: "staging", Actor: scopedAdmin.Identity}
	if promotion, err := scoped.promotions.AppendPromotion(t.Context(), spec); err != nil || promotion.RepoID != fixture.visible.ID {
		t.Fatalf("visible promotion refused: %+v, %v", promotion, err)
	}
	before, err := fixture.store.ListPromotions(t.Context(), model.PromotionListFilter{RepoID: fixture.hidden.ID})
	if err != nil {
		t.Fatal(err)
	}
	spec.RepoID = fixture.hidden.ID
	promotion, err := scoped.promotions.AppendPromotion(t.Context(), spec)
	if !errors.Is(err, store.ErrNotFound) || promotion.ID != "" {
		t.Fatalf("hidden repository promotion accepted: %+v, %v", promotion, err)
	}
	after, err := fixture.store.ListPromotions(t.Context(), model.PromotionListFilter{RepoID: fixture.hidden.ID})
	if err != nil || len(after.Promotions) != len(before.Promotions) {
		t.Fatalf("refused promotion changed hidden repository: %+v, %v", after, err)
	}
}
