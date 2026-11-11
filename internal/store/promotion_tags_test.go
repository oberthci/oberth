package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// promote tag_after requests (#1524): recorded once, retried while pending,
// settled once with an audit entry.
func TestPromotionTagRequestRetryAndSettle(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	promotion, err := s.AppendPromotion(t.Context(), model.PromotionSpec{RepoID: repo.ID, SourceBranch: "feature/a", SourceSHA: strings.Repeat("a", 40), TargetRef: "main", Actor: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPromotionTag(t.Context(), promotion.ID, "dev", "deploy/1", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty message: %v", err)
	}
	for range 2 {
		if err := s.RequestPromotionTag(t.Context(), promotion.ID, "dev", "deploy/1", "apply"); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := s.PendingPromotionTags(t.Context(), 10)
	if err != nil || len(pending) != 1 || pending[0].Name != "deploy/1" || pending[0].Status != model.PromotionTagPending {
		t.Fatalf("pending = %+v %v", pending, err)
	}
	next := now.Add(2 * time.Minute)
	retried, err := s.RetryPromotionTag(t.Context(), promotion.ID, 1, next, "transient")
	if err != nil || retried.Attempts != 1 || retried.NextRetryAt == nil || !retried.NextRetryAt.Equal(next) || retried.Status != model.PromotionTagPending {
		t.Fatalf("retry = %+v %v", retried, err)
	}
	settled, err := s.FinishPromotionTag(t.Context(), promotion.ID, model.PromotionTagPushed, "pushed", strings.Repeat("e", 40), 2)
	if err != nil || settled.Status != model.PromotionTagPushed || settled.ObjectSHA != strings.Repeat("e", 40) || settled.NextRetryAt != nil || settled.Attempts != 2 {
		t.Fatalf("settled = %+v %v", settled, err)
	}
	again, err := s.FinishPromotionTag(t.Context(), promotion.ID, model.PromotionTagFailed, "late", "", 3)
	if err != nil || again.Status != model.PromotionTagPushed {
		t.Fatalf("settled request changed: %+v %v", again, err)
	}
	actions, _, err := s.ListAuditActions(t.Context(), model.AuditListFilter{ActionPrefix: "promotion.tag_after.", Limit: 10})
	if err != nil || len(actions) != 2 {
		t.Fatalf("audit = %d %v", len(actions), err)
	}
	if pending, err := s.PendingPromotionTags(t.Context(), 10); err != nil || len(pending) != 0 {
		t.Fatalf("still pending = %+v %v", pending, err)
	}
}
