package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestResolveRunIDUsesUniquePrefixesAcrossRepositories(t *testing.T) {
	now := time.Now()
	database := testStore(t, &now)
	repo := createRepo(t, database)
	ctx := context.Background()
	firstID := "628316044e8b" + strings.Repeat("a", 20)
	secondID := "628316044e8b" + strings.Repeat("b", 20)
	enqueue := func(repoID int64, id string) {
		t.Helper()
		spec, err := validateRunSpec(testRunSpec(repoID, "feature/prefix", strings.Repeat("c", 40)))
		if err != nil {
			t.Fatal(err)
		}
		tx, err := database.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		// Choose IDs at insertion to exercise an actual collision without
		// altering immutable history or disabling foreign-key checks.
		if _, err := database.enqueueRunTx(ctx, tx, spec, id, unixNano(now)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	enqueue(repo.ID, firstID)
	for _, id := range []string{firstID, firstID[:12], firstID[:31], " " + strings.ToUpper(firstID[:12]) + " "} {
		got, err := database.ResolveRunID(ctx, id)
		if err != nil || got.ID != firstID {
			t.Fatalf("unique %q = %s, %v", id, got.ID, err)
		}
	}
	if _, err := database.Run(ctx, firstID[:12]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("exact mutation lookup accepted prefix: %v", err)
	}
	other, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "another", UpstreamID: repo.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	enqueue(other.ID, secondID)
	if _, err := database.ResolveRunID(ctx, firstID[:12]); !errors.Is(err, ErrAmbiguous) || !strings.Contains(err.Error(), "full run ID") {
		t.Fatalf("cross-repository collision = %v, want actionable ambiguity", err)
	}
	for _, id := range []string{firstID, secondID, firstID[:13], secondID[:13]} {
		got, err := database.ResolveRunID(ctx, id)
		if err != nil || !strings.HasPrefix(got.ID, id) {
			t.Fatalf("disambiguated %q = %s, %v", id, got.ID, err)
		}
	}
	for _, id := range []string{strings.Repeat("f", 12), strings.Repeat("f", 32)} {
		if _, err := database.ResolveRunID(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing %q = %v", id, err)
		}
	}
	for _, id := range []string{"", "628316044e8", "628316044e8%", "628316044e8_", "../628316044e8b", strings.Repeat("a", 33)} {
		if _, err := database.ResolveRunID(ctx, id); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid %q = %v", id, err)
		}
	}
}
