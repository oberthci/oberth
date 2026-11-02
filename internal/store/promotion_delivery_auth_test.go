package store

import (
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestPromotionDeliveryRechecksReservedGrant(t *testing.T) {
	for _, stage := range []string{"before-intent", "before-attempt", "during-push"} {
		for _, revoke := range []bool{false, true} {
			t.Run(stage+"/"+map[bool]string{false: "expired", true: "revoked"}[revoke], func(t *testing.T) {
				now := time.Now().UTC()
				s := testStore(t, &now)
				repo := createRepo(t, s)
				grant, err := s.AppendPromotionGrant(t.Context(), model.PromotionGrantSpec{RepoID: repo.ID, SourceSHA: retryStoreSource, TargetRef: "main", Issuer: "operator", IssuerFingerprint: "SHA256:operator", ExpiresAt: now.Add(time.Hour)})
				if err != nil {
					t.Fatal(err)
				}
				promotion, err := s.AppendPromotion(t.Context(), retryPromotionSpec(repo.ID, grant.ID))
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.PlanPromotion(t.Context(), promotion.ID, retryStoreBase, retryStoreSource); err != nil {
					t.Fatal(err)
				}
				invalidate := func() {
					if revoke {
						if _, err := s.RevokePromotionGrant(t.Context(), grant.ID, "operator"); err != nil {
							t.Fatal(err)
						}
					} else {
						now = now.Add(time.Hour)
					}
				}
				if stage == "before-intent" {
					invalidate()
				}
				publication, err := s.BeginPublication(t.Context(), model.PublicationSpec{RepoID: repo.ID, PromotionID: promotion.ID, RefKind: model.RefBranch, Ref: "main", PreviousSHA: retryStoreBase, PreviousKnown: true, ResultSHA: retryStoreSource, Actor: promotion.Actor})
				if stage == "before-intent" {
					if !errors.Is(err, ErrInvalidState) {
						t.Fatalf("invalid grant began publication: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if stage == "before-attempt" {
					invalidate()
				}
				_, err = s.UpdatePublicationAttempt(t.Context(), publication.ID, 0, 1, now.Add(time.Minute), "attempt")
				if stage == "before-attempt" {
					if !errors.Is(err, ErrInvalidState) {
						t.Fatalf("invalid grant began push attempt: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				invalidate()
				// A push already in flight may have succeeded. Preserve that fact,
				// while keeping the invalidated grant unconsumed.
				if _, err = s.FinalizePublication(t.Context(), publication.ID, model.PublicationDelivered, ""); err != nil {
					t.Fatal(err)
				}
				stored, err := s.PromotionGrant(t.Context(), grant.ID)
				if err != nil || stored.ConsumedAt != nil {
					t.Fatalf("invalidated grant consumed after delivery: %+v, %v", stored, err)
				}
			})
		}
	}
}
