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
		if _, err := database.enqueueRunTx(ctx, tx, spec, id, unixNano(now), 0); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	enqueue(repo.ID, firstID)
	for _, id := range []string{firstID, firstID[:8], firstID[:12], firstID[:31], " " + strings.ToUpper(firstID[:8]) + " "} {
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
	if _, err := database.ResolveRunID(ctx, firstID[:8]); !errors.Is(err, ErrAmbiguous) || !strings.Contains(err.Error(), firstID) || !strings.Contains(err.Error(), secondID) {
		t.Fatalf("cross-repository collision = %v, want actionable ambiguity", err)
	}
	thirdID := firstID[:8] + strings.Repeat("d", 24)
	enqueue(repo.ID, thirdID)
	_, err = database.ResolveRunID(ctx, firstID[:8])
	want := ErrAmbiguous.Error() + fmt.Sprintf(": run ID prefix %q matches: %s, %s, %s; use the full run ID", firstID[:8], firstID, secondID, thirdID)
	if err == nil || err.Error() != want {
		t.Fatalf("ambiguity must list all matching IDs only: %v, want %s", err, want)
	}
	// Hidden matches neither cause ambiguity nor appear in an error.
	got, err := database.ResolveRunIDIn(ctx, firstID[:8], []int64{other.ID})
	if err != nil || got.ID != secondID {
		t.Fatalf("scope-local unique prefix = %s, %v", got.ID, err)
	}
	_, err = database.ResolveRunIDIn(ctx, firstID[:8], []int64{repo.ID})
	want = ErrAmbiguous.Error() + fmt.Sprintf(": run ID prefix %q matches: %s, %s; use the full run ID", firstID[:8], firstID, thirdID)
	if err == nil || err.Error() != want {
		t.Fatalf("scope-local ambiguity = %v, want %s", err, want)
	}
	for _, id := range []string{secondID[:13], secondID} {
		_, hiddenErr := database.ResolveRunIDIn(ctx, id, []int64{repo.ID})
		_, absentErr := database.ResolveRunIDIn(ctx, id, []int64{})
		if !errors.Is(hiddenErr, ErrNotFound) || absentErr == nil || hiddenErr.Error() != absentErr.Error() {
			t.Fatalf("hidden %s = %v; absent = %v", id, hiddenErr, absentErr)
		}
	}
	if got, err := database.ResolveRunIDIn(ctx, secondID, []int64{other.ID}); err != nil || got.ID != secondID {
		t.Fatalf("scoped full ID = %s, %v", got.ID, err)
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
	for _, id := range []string{"", "6283160", "628316044e8%", "628316044e8_", "../628316044e8b", strings.Repeat("a", 33)} {
		if _, err := database.ResolveRunID(ctx, id); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid %q = %v", id, err)
		}
	}
}

func TestRunIDsExistReportsEachID(t *testing.T) {
	now := time.Now()
	database := testStore(t, &now)
	repo := createRepo(t, database)
	run, err := database.EnqueueRun(context.Background(), testRunSpec(repo.ID, "main", strings.Repeat("c", 40)))
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Repeat("0", 32)
	exists, err := database.RunIDsExist(context.Background(), []string{run.ID, unknown})
	if err != nil {
		t.Fatal(err)
	}
	if !exists[run.ID] || exists[unknown] || len(exists) != 2 {
		t.Fatalf("exists = %v", exists)
	}
}
