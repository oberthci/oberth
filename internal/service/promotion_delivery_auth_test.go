package service

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestPublicationDoesNotPushWithRevokedReservedGrant(t *testing.T) {
	f := newControlFixture(t)
	source, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	grant, err := f.store.AppendPromotionGrant(t.Context(), model.PromotionGrantSpec{RepoID: f.repo.ID, SourceSHA: source, TargetRef: "main", Issuer: "operator", IssuerFingerprint: "SHA256:operator", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	promotion, err := f.store.AppendPromotion(t.Context(), model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: "feature/expiry", SourceSHA: source, TargetRef: "main", Actor: "worker", GrantID: grant.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PlanPromotion(t.Context(), promotion.ID, base, source); err != nil {
		t.Fatal(err)
	}
	publication, err := f.store.BeginPublication(t.Context(), model.PublicationSpec{RepoID: f.repo.ID, PromotionID: promotion.ID, RefKind: model.RefBranch, Ref: "main", PreviousSHA: base, PreviousKnown: true, ResultSHA: source, Actor: promotion.Actor})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RevokePromotionGrant(t.Context(), grant.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	f.git.mu.Lock()
	f.git.setRemoteRefLocked("refs/heads/main", base)
	f.git.mu.Unlock()
	git := &retryPublicationGit{controlGit: f.git}
	final, err := deliverPublication(t.Context(), f.store, git, f.store, f.repo, f.repo.Name, publication)
	if err != nil || git.calls != 0 || final.Publication.Status != model.PublicationFailed || final.Promotion.Status != model.PromotionFailed {
		t.Fatalf("invalid grant reached push: calls=%d result=%+v error=%v", git.calls, final, err)
	}
	stored, err := f.store.PromotionGrant(t.Context(), grant.ID)
	if err != nil || stored.ConsumedAt != nil {
		t.Fatalf("refused grant consumed: %+v %v", stored, err)
	}
}
