package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestHasPassedRunForSHAIsScopedToFragmentRepository(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()
	upstream, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "forgejo", BaseURL: "https://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "fragment", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if registered, err := s.RepositoryRegistered(ctx, "ACME/fragment"); err != nil || !registered {
		t.Fatalf("case-insensitive fragment org lookup = %v, %v", registered, err)
	}
	other, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "other", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	passed, err := s.HasPassedRunForSHA(ctx, "acme/fragment", sha)
	if err != nil || passed {
		t.Fatalf("untested fragment = %v, %v", passed, err)
	}
	for _, repoID := range []int64{other.ID, fragment.ID} {
		run, enqueueErr := s.EnqueueRun(ctx, model.RunSpec{RepoID: repoID, RefKind: model.RefBranch, Ref: "feature/fragment", SHA: sha, TestedSHA: sha, Actor: "tester", Trigger: "branch"})
		if enqueueErr != nil {
			t.Fatal(enqueueErr)
		}
		if _, finishErr := s.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed}); finishErr != nil {
			t.Fatal(finishErr)
		}
		passed, err = s.HasPassedRunForSHA(ctx, "acme/fragment", sha)
		if err != nil || passed != (repoID == fragment.ID) {
			t.Fatalf("fragment pass after repo %d = %v, %v", repoID, passed, err)
		}
		if repoID == fragment.ID {
			if folded, lookupErr := s.HasPassedRunForSHA(ctx, "ACME/fragment", sha); lookupErr != nil || !folded {
				t.Fatalf("case-insensitive green run lookup = %v, %v", folded, lookupErr)
			}
		}
	}
	if passed, err := s.HasPassedRunForSHA(ctx, "acme/fragment", sha[:12]); err == nil || passed {
		t.Fatalf("abbreviated SHA accepted: %v, %v", passed, err)
	}
}
