package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestListPromotionsFindsPendingAdmissionBeyondRecentGlobalPage(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	other, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "other", UpstreamID: repo.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	appendPromotion := func(repoID int64, sha string) model.Promotion {
		t.Helper()
		p, err := s.AppendPromotion(ctx, model.PromotionSpec{RepoID: repoID, SourceBranch: "feature", SourceSHA: sha, TargetRef: "main", Actor: "test"})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	sha := strings.Repeat("a", 40)
	oldest := appendPromotion(repo.ID, sha)
	failed := appendPromotion(repo.ID, sha)
	if _, err := s.FinishPromotion(ctx, failed.ID, model.PromotionFailed, "", "planning failed"); err != nil {
		t.Fatal(err)
	}
	newest := appendPromotion(repo.ID, sha)
	for i := 0; i < 205; i++ {
		appendPromotion(other.ID, fmt.Sprintf("%040x", i+1))
	}
	page, err := s.ListPromotions(ctx, model.PromotionListFilter{RepoID: repo.ID, SourceSHA: sha, Status: model.PromotionPending, Limit: 1})
	if err != nil || len(page.Promotions) != 1 || page.Promotions[0].ID != newest.ID || page.NextBefore != newest.Sequence {
		t.Fatalf("first filtered page = %#v, %v", page, err)
	}
	page, err = s.ListPromotions(ctx, model.PromotionListFilter{RepoID: repo.ID, SourceSHA: sha, Status: model.PromotionPending, Limit: 1, Before: page.NextBefore})
	if err != nil || len(page.Promotions) != 1 || page.Promotions[0].ID != oldest.ID || page.NextBefore != 0 || page.Promotions[0].RunID != "" || page.Promotions[0].ResultSHA != "" {
		t.Fatalf("lost ID admission = %#v, %v", page, err)
	}
	for _, tc := range []struct{ limit, want int }{{0, 50}, {200, 200}} {
		page, err := s.ListPromotions(ctx, model.PromotionListFilter{Limit: tc.limit})
		if err != nil || len(page.Promotions) != tc.want || page.NextBefore == 0 {
			t.Fatalf("bounded global page = %#v, %v", page, err)
		}
	}
	page, err = s.ListPromotions(ctx, model.PromotionListFilter{RepoID: repo.ID, SourceSHA: strings.Repeat("f", 64)})
	if err != nil || page.Promotions == nil || len(page.Promotions) != 0 || page.NextBefore != 0 {
		t.Fatalf("empty page = %#v, %v", page, err)
	}
	for _, filter := range []model.PromotionListFilter{{RepoID: -1}, {Before: -1}, {Limit: -1}, {Limit: 201}, {Status: "running"}, {SourceSHA: "aaaaaaa"}, {SourceSHA: strings.Repeat("A", 40)}} {
		if _, err := s.ListPromotions(ctx, filter); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid filter %#v returned %v", filter, err)
		}
	}
}
