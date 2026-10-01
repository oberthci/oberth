package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// TestAccessListResolvesRepoSpelling is the #256 regression test: persisted
// grant rows key on the qualified "upstream/org/repo" form (schema v12 +
// reconciler canonicalization), so the access_list repo filter must resolve
// every accepted spelling onto that key. Before the fix, a bare or
// org-qualified filter was string-matched verbatim and misreported an empty
// grant set for a repository that holds release grants.
func TestAccessListResolvesRepoSpelling(t *testing.T) {
	t.Parallel()
	fixture := newControlFixture(t)
	ctx := context.Background()

	service, err := NewAPI(APIConfig{
		Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
		Issues: fixture.store, Promotions: fixture.store, PromotionRuns: fixture.store,
		Enqueues: fixture.scheduler, Git: fixture.git, Refs: fixture.refs,
		Logs: fixture.logs, Auditor: fixture.store,
		Signals: fixture.signals, MaximumWait: 50 * time.Millisecond,
		PromotionWorkspaceRoot: filepath.Join(fixture.root, "promotion-work"),
		SecretAccess:           fixture.store,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The persisted row carries the qualified key, exactly as the v12
	// migration and the canonicalizing reconciler write it.
	qualified, err := fixture.store.QualifiedRepoName(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Grant(ctx, qualified, "*", "oberth/data/release/cosign-secret", "admin@test"); err != nil {
		t.Fatal(err)
	}

	// Every accepted spelling of the repository must find the grant.
	for _, spelling := range []string{"oberth", "acme/oberth", "codeberg/acme/oberth", qualified} {
		response, err := service.accessList(ctx, spelling, false)
		if err != nil {
			t.Fatalf("accessList(%q): %v", spelling, err)
		}
		if len(response.Grants) != 1 {
			t.Fatalf("accessList(%q) returned %d grants, want 1 (spelling not resolved onto the qualified key)", spelling, len(response.Grants))
		}
		if response.Grants[0].Repo != qualified {
			t.Fatalf("accessList(%q) grant repo = %q, want %q", spelling, response.Grants[0].Repo, qualified)
		}
	}

	// The unfiltered list is unchanged.
	response, err := service.accessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Grants) != 1 {
		t.Fatalf("unfiltered accessList returned %d grants, want 1", len(response.Grants))
	}

	// An unregistered spelling stays verbatim and simply matches nothing —
	// never an error, never another repo's grants.
	response, err = service.accessList(ctx, "not-registered", false)
	if err != nil {
		t.Fatalf("accessList(unregistered): %v", err)
	}
	if len(response.Grants) != 0 {
		t.Fatalf("unregistered filter returned %d grants, want 0", len(response.Grants))
	}

	// Once a same-name repository exists under a second upstream, the bare
	// spelling names neither: refuse it, and keep both qualified spellings
	// resolving to their own repository only.
	upstream2, err := fixture.store.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/beta",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.CreateRepository(ctx, model.RepositorySpec{
		Name: "oberth", UpstreamID: upstream2.ID, DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.accessList(ctx, "oberth", false); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("ambiguous bare filter error = %v, want ErrInvalidInput", err)
	}
	response, err = service.accessList(ctx, "codeberg/acme/oberth", false)
	if err != nil || len(response.Grants) != 1 {
		t.Fatalf("qualified filter after twin registration: grants=%d err=%v, want 1/nil", len(response.Grants), err)
	}
	response, err = service.accessList(ctx, "github/beta/oberth", false)
	if err != nil || len(response.Grants) != 0 {
		t.Fatalf("twin repo filter: grants=%d err=%v, want 0/nil (no aliasing)", len(response.Grants), err)
	}
}
