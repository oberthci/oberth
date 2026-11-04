package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestParkTrainChecksLaterGrantWhenFirstSourceUsesOptIn(t *testing.T) {
	for _, mode := range []string{"active", "revoked", "expired"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			first, second := strings.Repeat("a", 40), strings.Repeat("b", 40)
			g, err := s.AppendPromotionGrant(t.Context(), model.PromotionGrantSpec{RepoID: repo.ID, SourceSHA: second, TargetRef: "main", Issuer: "admin", IssuerFingerprint: "SHA256:admin", ExpiresAt: now.Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "revoked" {
				if _, err := s.RevokePromotionGrant(t.Context(), g.ID, "admin"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "expired" {
				now = now.Add(2 * time.Hour)
			}
			spec := model.PromotionSpec{RepoID: repo.ID, SourceSHA: first, SourceBranch: "park/review", TargetRef: "main", Actor: "developer", BranchCISkipped: true, Train: &model.PromotionTrain{Sources: []model.PromotionSource{{SHA: first, Ref: "park/review"}, {SHA: second, Ref: "feature/tested", GrantID: g.ID}}}}
			p, err := s.AppendPromotion(t.Context(), spec)
			if mode != "active" {
				if !errors.Is(err, ErrInvalidState) {
					t.Fatalf("later %s grant was not checked: %v", mode, err)
				}
				return
			}
			if err != nil {
				t.Fatal("mixed authority refused", err)
			}
			audit, err := s.LatestAuditActionByType(t.Context(), "promotion_grant.reserve", "promotion_grant")
			if err != nil || audit.ResourceID != g.ID {
				t.Fatal("later grant was not audited", audit, err)
			}
			if _, err := s.AppendPromotion(t.Context(), spec); !errors.Is(err, ErrInvalidState) {
				t.Fatal("later grant was not reserved", err)
			}
			if _, err := s.RevokePromotionGrant(t.Context(), g.ID, "admin"); err != nil {
				t.Fatal(err)
			}
			tx, err := s.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if err := checkPromotionDeliveryGrantTx(t.Context(), tx, p, unixNano(now)); !errors.Is(err, ErrPromotionGrantInactive) {
				t.Fatal("later grant revocation ignored at delivery", err)
			}
		})
	}
}
