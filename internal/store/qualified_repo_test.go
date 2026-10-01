package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// TestRepositoryByNameAllThreeForms verifies that RepositoryByName accepts
// bare, org-qualified, and upstream-qualified name forms and resolves each
// to the same repository when the name is unambiguous.
func TestRepositoryByNameAllThreeForms(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	upstream, err := s.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/oberthci",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.CreateRepository(ctx, model.RepositorySpec{
		Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Bare name.
	found, err := s.RepositoryByName(ctx, "oberth")
	if err != nil {
		t.Fatalf("bare lookup: %v", err)
	}
	if found.ID != repo.ID {
		t.Fatalf("bare lookup returned repo %d, want %d", found.ID, repo.ID)
	}

	// Org-qualified: "oberthci/oberth".
	found, err = s.RepositoryByName(ctx, "oberthci/oberth")
	if err != nil {
		t.Fatalf("org-qualified lookup: %v", err)
	}
	if found.ID != repo.ID {
		t.Fatalf("org-qualified lookup returned repo %d, want %d", found.ID, repo.ID)
	}

	// Upstream-qualified: "github/oberthci/oberth".
	found, err = s.RepositoryByName(ctx, "github/oberthci/oberth")
	if err != nil {
		t.Fatalf("upstream-qualified lookup: %v", err)
	}
	if found.ID != repo.ID {
		t.Fatalf("upstream-qualified lookup returned repo %d, want %d", found.ID, repo.ID)
	}
}

// TestRepositoryByNameAmbiguousBareName verifies that a bare name that
// exists under multiple upstreams returns ErrAmbiguous.
func TestRepositoryByNameAmbiguousBareName(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	upstream1, err := s.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	upstream2, err := s.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/oberthci",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateRepository(ctx, model.RepositorySpec{
		Name: "terraform", UpstreamID: upstream1.ID, DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	repo2, err := s.CreateRepository(ctx, model.RepositorySpec{
		Name: "terraform", UpstreamID: upstream2.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatalf("same-name under different upstream should succeed: %v", err)
	}

	// Bare name: ambiguous.
	_, err = s.RepositoryByName(ctx, "terraform")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("bare lookup for ambiguous name = %v, want ErrAmbiguous", err)
	}

	// Org-qualified: resolves to correct repo.
	found, err := s.RepositoryByName(ctx, "oberthci/terraform")
	if err != nil {
		t.Fatalf("org-qualified lookup: %v", err)
	}
	if found.ID != repo2.ID {
		t.Fatalf("org-qualified lookup returned repo %d, want %d", found.ID, repo2.ID)
	}

	// Upstream-qualified: resolves to correct repo.
	found, err = s.RepositoryByName(ctx, "codeberg/acme/terraform")
	if err != nil {
		t.Fatalf("upstream-qualified lookup: %v", err)
	}
	if found.UpstreamID != upstream1.ID {
		t.Fatalf("upstream-qualified lookup upstream = %d, want %d", found.UpstreamID, upstream1.ID)
	}
}

// TestRepositoryByNameNotFoundCases verifies error handling for non-existent
// repositories in all three name forms.
func TestRepositoryByNameNotFoundCases(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	if _, err := s.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/oberthci",
	}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"nonexistent",
		"oberthci/nonexistent",
		"github/oberthci/nonexistent",
		"nosuch/org/repo",
	} {
		_, err := s.RepositoryByName(ctx, name)
		if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrInvalid) {
			t.Errorf("RepositoryByName(%q) = %v, want ErrNotFound or ErrInvalid", name, err)
		}
	}
}

// TestRepositoryByNameUpstreamOrgMismatch verifies that a fully qualified
// name with an org that doesn't match the upstream's org returns an error.
func TestRepositoryByNameUpstreamOrgMismatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 13, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	upstream, err := s.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/oberthci",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRepository(ctx, model.RepositorySpec{
		Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	// Wrong org for this upstream.
	_, err = s.RepositoryByName(ctx, "github/wrongorg/oberth")
	if err == nil {
		t.Fatal("expected error for org mismatch")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("org mismatch error = %v, want ErrInvalid", err)
	}
}

// TestRepositoryRegisteredHandlesAllForms verifies that RepositoryRegistered
// works with all three name forms.
func TestRepositoryRegisteredHandlesAllForms(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	upstream, err := s.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/oberthci",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRepository(ctx, model.RepositorySpec{
		Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		expected bool
	}{
		{"oberth", true},
		{"oberthci/oberth", true},
		{"github/oberthci/oberth", true},
		{"nonexistent", false},
		{"oberthci/nonexistent", false},
	} {
		registered, err := s.RepositoryRegistered(ctx, tc.name)
		if err != nil {
			t.Errorf("RepositoryRegistered(%q): %v", tc.name, err)
			continue
		}
		if registered != tc.expected {
			t.Errorf("RepositoryRegistered(%q) = %v, want %v", tc.name, registered, tc.expected)
		}
	}
}

// TestCompoundUniqueAllowsSameNameDifferentUpstream verifies the G3 schema
// change: UNIQUE(upstream_id, name) allows two repos with the same name
// under different upstreams.
func TestCompoundUniqueAllowsSameNameDifferentUpstream(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 15, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	upstream1, err := s.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	upstream2, err := s.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/oberthci",
	})
	if err != nil {
		t.Fatal(err)
	}

	repo1, err := s.CreateRepository(ctx, model.RepositorySpec{
		Name: "shared-name", UpstreamID: upstream1.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo2, err := s.CreateRepository(ctx, model.RepositorySpec{
		Name: "shared-name", UpstreamID: upstream2.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatalf("same name, different upstream should succeed: %v", err)
	}
	if repo1.ID == repo2.ID {
		t.Fatal("different repos should have different IDs")
	}

	// Same name, same upstream: must still fail.
	_, err = s.CreateRepository(ctx, model.RepositorySpec{
		Name: "shared-name", UpstreamID: upstream1.ID, DefaultBranch: "main",
	})
	if err == nil {
		t.Fatal("same name + same upstream should fail")
	}
}

// TestScheduleFiresUseQualifiedNames verifies that after the v10 migration,
// schedule fires use org-qualified repo keys.
func TestScheduleFiresUseQualifiedNames(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 16, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	qualifiedName := "github/oberthci/oberth"
	if err := s.RecordScheduleFire(ctx, qualifiedName, "nightly", now, "fired"); err != nil {
		t.Fatalf("record schedule fire with qualified name: %v", err)
	}

	fires, err := s.ScheduleFires(ctx, qualifiedName)
	if err != nil {
		t.Fatalf("read schedule fires: %v", err)
	}
	if len(fires) != 1 {
		t.Fatalf("schedule fires count = %d, want 1", len(fires))
	}
	if _, ok := fires["nightly"]; !ok {
		t.Fatal("nightly entry not found in schedule fires")
	}

	// Bare name should not match.
	bareFires, err := s.ScheduleFires(ctx, "oberth")
	if err != nil {
		t.Fatal(err)
	}
	if len(bareFires) != 0 {
		t.Fatalf("bare name should not match qualified fires, got %d", len(bareFires))
	}
}
