package cronsync_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/cronsync/fake"
	"github.com/oberthci/oberth/internal/gitport"
)

// cachedGit counts refreshing resolves and cached ones.
type cachedGit struct {
	*fake.Git
	refreshing, cached atomic.Int32
}

func (g *cachedGit) ResolveRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	g.refreshing.Add(1)
	return g.Git.ResolveRef(ctx, repo, ref)
}

func (g *cachedGit) CachedRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	g.cached.Add(1)
	return g.Git.ResolveRef(ctx, repo, ref)
}

// The relevance check of a ref notice reads Git without refreshing, so a
// forge move it would fetch is not taken away from the notice that reports it;
// a relevant notice still reconciles with fresh reads.
func TestRefsChangedRelevanceReadsWithoutRefreshing(t *testing.T) {
	f := newFixture(t)
	git := &cachedGit{Git: f.git}
	config := f.config
	config.Git = git
	c, err := cronsync.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.RefsChanged(t.Context(), f.repo.Qualified, []string{"refs/heads/unrelated"}); err != nil {
		t.Fatal(err)
	}
	if git.refreshing.Load() != 0 || git.cached.Load() == 0 {
		t.Fatal("the relevance check refreshed", git.refreshing.Load(), git.cached.Load())
	}
	if err = c.RefsChanged(t.Context(), f.repo.Qualified, []string{"refs/heads/main"}); err != nil {
		t.Fatal(err)
	}
	if git.refreshing.Load() == 0 {
		t.Fatal("a relevant notice did not reconcile with fresh reads")
	}
}
