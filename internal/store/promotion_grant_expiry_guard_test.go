package store

import (
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestPromotionGrantRefusesNonFutureExpiry(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	upstream, err := database.CreateUpstream(t.Context(), model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/oberthci"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := database.CreateRepository(t.Context(), model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	spec := model.PromotionGrantSpec{RepoID: repository.ID, SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TargetRef: "main",
		Issuer: "operator", IssuerFingerprint: "SHA256:fixture", ExpiresAt: now.Add(time.Hour)}
	valid, err := database.AppendPromotionGrant(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, expires := range []time.Time{now, now.Add(-time.Nanosecond)} {
		spec.ExpiresAt = expires
		if grant, err := database.AppendPromotionGrant(t.Context(), spec); !errors.Is(err, ErrInvalid) || grant.ID != "" {
			t.Fatalf("non-future grant accepted: %+v, %v", grant, err)
		}
	}
	grants, err := database.ListPromotionGrants(t.Context(), model.PromotionGrantListFilter{RepoID: repository.ID})
	if err != nil || len(grants) != 1 || grants[0].ID != valid.ID {
		t.Fatalf("refused grants changed persisted authority: %+v, %v", grants, err)
	}
}
