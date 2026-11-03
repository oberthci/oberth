package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestPromotionPrefixResolutionAndScope(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	other, err := s.CreateRepository(t.Context(), model.RepositorySpec{Name: "other", UpstreamID: repo.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	resolver, ok := any(s).(interface {
		ResolvePromotionIDIn(context.Context, string, []int64) (model.Promotion, error)
	})
	if !ok {
		t.Fatal("promotion prefix resolver is missing")
	}
	ids := []string{"1234abcd5678" + strings.Repeat("a", 20), "1234abcd5678" + strings.Repeat("b", 20)}
	for i, repoID := range []int64{repo.ID, other.ID} {
		spec := model.PromotionSpec{RepoID: repoID, SourceBranch: "feature/prefix", SourceSHA: strings.Repeat("c", 40), TargetRef: "main", Actor: "fixture"}
		if _, err := insertPromotion(t.Context(), s.db, spec, ids[i], "", "", unixNano(now)); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []int{8, 12} {
		_, err := resolver.ResolvePromotionIDIn(t.Context(), ids[0][:n], nil)
		if !errors.Is(err, ErrAmbiguous) || !strings.Contains(err.Error(), ids[0]) || !strings.Contains(err.Error(), ids[1]) {
			t.Fatalf("ambiguity: %v", err)
		}
		got, err := resolver.ResolvePromotionIDIn(t.Context(), ids[0][:n], []int64{repo.ID})
		if err != nil || got.ID != ids[0] {
			t.Fatalf("scoped prefix: %+v %v", got, err)
		}
		_, err = resolver.ResolvePromotionIDIn(t.Context(), ids[0][:n], []int64{})
		if !errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), ids[0]) {
			t.Fatalf("hidden prefix: %v", err)
		}
	}
	for _, id := range []string{ids[0], strings.ToUpper(ids[0][:13])} {
		got, err := resolver.ResolvePromotionIDIn(t.Context(), id, nil)
		if err != nil || got.ID != ids[0] {
			t.Fatalf("unique prefix: %+v %v", got, err)
		}
	}
	for _, id := range []string{"1234567", "gggggggg", strings.Repeat("a", 33)} {
		if _, err := resolver.ResolvePromotionIDIn(t.Context(), id, nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid %q: %v", id, err)
		}
	}
	if _, err := s.Promotion(t.Context(), ids[0][:12]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("internal exact lookup abbreviated: %v", err)
	}
}
