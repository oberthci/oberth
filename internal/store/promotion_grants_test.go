package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestPromotionGrantAppendAndMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)
	upstream, _ := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	repo, _ := s.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})

	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	expires := now.Add(1 * time.Hour)

	// Create a grant.
	grant, err := s.AppendPromotionGrant(ctx, model.PromotionGrantSpec{
		RepoID: repo.ID, SourceSHA: sha, TargetRef: "main",
		Issuer: "admin@host", IssuerFingerprint: "SHA256:admin",
		Note: "test grant", ExpiresAt: expires,
	})
	if err != nil {
		t.Fatalf("AppendPromotionGrant error = %v", err)
	}
	if grant.ID == "" || grant.RepoID != repo.ID || grant.SourceSHA != sha || grant.TargetRef != "main" {
		t.Fatalf("grant = %+v", grant)
	}
	if grant.RevokedAt != nil || grant.ConsumedAt != nil {
		t.Fatal("new grant should not be revoked or consumed")
	}

	// Match the grant.
	matched, err := s.MatchingPromotionGrant(ctx, repo.ID, sha, "main")
	if err != nil {
		t.Fatalf("MatchingPromotionGrant error = %v", err)
	}
	if matched.ID != grant.ID {
		t.Fatalf("matched grant ID = %s, want %s", matched.ID, grant.ID)
	}

	// No match for wrong target.
	_, err = s.MatchingPromotionGrant(ctx, repo.ID, sha, "staging")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("MatchingPromotionGrant wrong target error = %v, want ErrNotFound", err)
	}

	// No match for wrong SHA.
	_, err = s.MatchingPromotionGrant(ctx, repo.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "main")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("MatchingPromotionGrant wrong SHA error = %v, want ErrNotFound", err)
	}
}

func TestPromotionGrantRevoke(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)
	upstream, _ := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	repo, _ := s.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})

	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	grant, err := s.AppendPromotionGrant(ctx, model.PromotionGrantSpec{
		RepoID: repo.ID, SourceSHA: sha, TargetRef: "main",
		Issuer: "admin@host", IssuerFingerprint: "SHA256:admin",
		ExpiresAt: now.Add(1 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Revoke the grant.
	revoked, err := s.RevokePromotionGrant(ctx, grant.ID, "admin@host")
	if err != nil {
		t.Fatalf("RevokePromotionGrant error = %v", err)
	}
	if revoked.RevokedAt == nil || revoked.RevokedBy != "admin@host" {
		t.Fatalf("revoked grant = %+v", revoked)
	}

	// Revoked grant should not match.
	_, err = s.MatchingPromotionGrant(ctx, repo.ID, sha, "main")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("MatchingPromotionGrant after revoke error = %v, want ErrNotFound", err)
	}

	// Double revoke should fail.
	_, err = s.RevokePromotionGrant(ctx, grant.ID, "admin@host")
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("double revoke error = %v, want ErrInvalidState", err)
	}
}

func TestPromotionGrantConsume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)
	upstream, _ := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	repo, _ := s.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})

	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	grant, err := s.AppendPromotionGrant(ctx, model.PromotionGrantSpec{
		RepoID: repo.ID, SourceSHA: sha, TargetRef: "main",
		Issuer: "admin@host", IssuerFingerprint: "SHA256:admin",
		ExpiresAt: now.Add(1 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Consume the grant.
	if err := s.ConsumePromotionGrant(ctx, grant.ID, "promotion-1", "agent@host"); err != nil {
		t.Fatalf("ConsumePromotionGrant error = %v", err)
	}

	// Consumed grant should not match.
	_, err = s.MatchingPromotionGrant(ctx, repo.ID, sha, "main")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("MatchingPromotionGrant after consume error = %v, want ErrNotFound", err)
	}

	// Double consume should fail.
	if err := s.ConsumePromotionGrant(ctx, grant.ID, "promotion-2", "agent@host"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("double consume error = %v, want ErrInvalidState", err)
	}
}

func TestPromotionGrantList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)
	upstream, _ := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	repo, _ := s.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})

	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for i := 0; i < 3; i++ {
		if _, err := s.AppendPromotionGrant(ctx, model.PromotionGrantSpec{
			RepoID: repo.ID, SourceSHA: sha, TargetRef: "main",
			Issuer: "admin@host", IssuerFingerprint: "SHA256:admin",
			ExpiresAt: now.Add(1 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}

	grants, err := s.ListPromotionGrants(ctx, model.PromotionGrantListFilter{RepoID: repo.ID})
	if err != nil {
		t.Fatalf("ListPromotionGrants error = %v", err)
	}
	if len(grants) != 3 {
		t.Fatalf("ListPromotionGrants count = %d, want 3", len(grants))
	}
}

func TestPromotionGrantAppendRejectsInvalidSpec(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)

	// Missing repo.
	_, err := s.AppendPromotionGrant(ctx, model.PromotionGrantSpec{
		RepoID: 0, SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetRef: "main", Issuer: "admin", IssuerFingerprint: "SHA256:x",
		ExpiresAt: now.Add(1 * time.Hour),
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing repo error = %v, want ErrInvalid", err)
	}

	// Short SHA.
	_, err = s.AppendPromotionGrant(ctx, model.PromotionGrantSpec{
		RepoID: 1, SourceSHA: "aabbcc", TargetRef: "main",
		Issuer: "admin", IssuerFingerprint: "SHA256:x",
		ExpiresAt: now.Add(1 * time.Hour),
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("short SHA error = %v, want ErrInvalid", err)
	}
}

func TestPromotionWithGrantIDPersists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)
	upstream, _ := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	repo, _ := s.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})

	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// A promotion can only hold a grant that exists (#1097).
	if _, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/test", SourceSHA: sha,
		TargetRef: "main", Actor: "agent@host", GrantID: "grant-1234",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AppendPromotion with an unknown grant error = %v, want ErrNotFound", err)
	}
	grant, err := s.AppendPromotionGrant(ctx, model.PromotionGrantSpec{
		RepoID: repo.ID, SourceSHA: sha, TargetRef: "main",
		Issuer: "admin@host", IssuerFingerprint: "SHA256:admin", ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	promotion, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/test", SourceSHA: sha,
		TargetRef: "main", Actor: "agent@host", GrantID: grant.ID,
	})
	if err != nil {
		t.Fatalf("AppendPromotion with GrantID error = %v", err)
	}
	if promotion.GrantID != grant.ID {
		t.Fatalf("promotion.GrantID = %q, want %q", promotion.GrantID, grant.ID)
	}

	// Verify persisted.
	reloaded, err := s.Promotion(ctx, promotion.ID)
	if err != nil {
		t.Fatalf("Promotion reload error = %v", err)
	}
	if reloaded.GrantID != grant.ID {
		t.Fatalf("reloaded promotion.GrantID = %q, want %q", reloaded.GrantID, grant.ID)
	}
}
