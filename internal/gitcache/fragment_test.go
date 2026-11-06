package gitcache

import (
	"context"
	"testing"
)

func TestTagSHAResolvesALightweightTag(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()

	want := commitFragment(t, repository, "fragment", "kind: Workflow\n")
	runGit(t, repository.work, "tag", "v3")
	runGit(t, repository.work, "push", "origin", "v3")
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}

	got, err := cache.TagSHA(ctx, "example", "v3")
	if err != nil {
		t.Fatalf("TagSHA: %v", err)
	}
	if got != want {
		t.Fatalf("TagSHA(v3) = %s, want %s", got, want)
	}
}

func TestTagSHAResolvesAnAnnotatedTagToItsCommit(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()

	want := commitFragment(t, repository, "fragment", "kind: Workflow\n")
	runGit(t, repository.work, "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
		"tag", "-a", "v4", "-m", "release four")
	runGit(t, repository.work, "push", "origin", "v4")
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}

	got, err := cache.TagSHA(ctx, "example", "v4")
	if err != nil {
		t.Fatalf("TagSHA: %v", err)
	}
	if got != want {
		t.Fatalf("TagSHA(v4) = %s, want the commit %s, not the tag object", got, want)
	}
}

func TestTagSHARefusesAMissingTagAndABranchName(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ctx := context.Background()
	if _, err := cache.Ensure(ctx, "example"); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.TagSHA(ctx, "example", "v9"); err == nil {
		t.Fatal("TagSHA resolved a tag that does not exist")
	}
	if _, err := cache.TagSHA(ctx, "example", "main"); err == nil {
		t.Fatal("TagSHA resolved a branch name; a fragment must be pinned to a tag")
	}
}

func TestTagObjectSHAKeepsAnnotatedTagIdentity(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	commit := commitFragment(t, repository, "fragment", "kind: Workflow\n")
	runGit(t, repository.work, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "tag", "-a", "v-object", "-m", "first object")
	object := runGit(t, repository.work, "rev-parse", "refs/tags/v-object")
	runGit(t, repository.work, "push", "origin", "v-object")
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	got, err := cache.TagObjectSHA(t.Context(), "example", "v-object")
	if err != nil || got != object || got == commit {
		t.Fatalf("tag object was peeled: %s object=%s commit=%s err=%v", got, object, commit, err)
	}
	peeled, err := cache.TagSHA(t.Context(), "example", "v-object")
	if err != nil || peeled != commit {
		t.Fatalf("existing fragment resolution changed: %s %v", peeled, err)
	}
	if _, err := cache.TagObjectSHA(t.Context(), "example", "missing"); err == nil {
		t.Fatal("missing tag accepted")
	}
}

func TestTagObjectSHARefusesBusyRepositoryLock(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	_, path, err := cache.path("example")
	if err != nil {
		t.Fatal(err)
	}
	lock := cache.repoLock(path)
	lock.Lock()
	defer lock.Unlock()
	if _, err := cache.TagObjectSHA(t.Context(), "example", "v-busy"); err == nil {
		t.Fatal("busy exact-tag lookup did not fail closed")
	}
}

func TestUpstreamFragmentTagsDoNotMaterializeOrChangeReplay(t *testing.T) {
	for _, kind := range []string{"lightweight", "annotated"} {
		t.Run(kind, func(t *testing.T) {
			repository := newTestRepository(t)
			cache := newTestCache(t, repository.upstream)
			original := commitFragment(t, repository, "original", "kind: Workflow\n")
			if kind == "annotated" {
				runGit(t, repository.work, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "tag", "-a", "v1", "-m", "original")
			} else {
				runGit(t, repository.work, "tag", "v1")
			}
			runGit(t, repository.work, "push", "origin", "v1")
			if _, err := cache.Ensure(t.Context(), "example"); err != nil {
				t.Fatal(err)
			}
			object, err := cache.TagObjectSHA(t.Context(), "example", "v1")
			if err != nil {
				t.Fatal(err)
			}
			upstream := WithUpstreamFragmentTags(WithFreshPolicy(t.Context()))
			refresh := func() {
				t.Helper()
				if _, _, _, err := cache.ReadDefaultBlob(upstream, "example", "fragment", 4096); err != nil {
					t.Fatal(err)
				}
			}
			refresh()
			if got, err := cache.TagSHA(upstream, "example", "v1"); err != nil || got != original {
				t.Fatal("unchanged upstream tag", got, err)
			}
			moved := commitFragment(t, repository, "moved", "kind: Workflow\n# next\n")
			runGit(t, repository.work, "tag", "-f", "v1")
			runGit(t, repository.work, "push", "origin", "+refs/tags/v1:refs/tags/v1")
			refresh()
			if got, err := cache.TagSHA(upstream, "example", "v1"); err != nil || got != moved {
				t.Fatal("moved upstream tag", got, err)
			}
			runGit(t, repository.work, "push", "origin", ":refs/tags/v1")
			refresh()
			if _, err := cache.TagSHA(upstream, "example", "v1"); err == nil {
				t.Fatal("deleted upstream tag resolved")
			}
			if got, err := cache.TagSHA(t.Context(), "example", "v1"); err != nil || got != original {
				t.Fatal("ordinary public tag changed", got, err)
			}
			if got, err := cache.TagObjectSHA(upstream, "example", "v1"); err != nil || got != object {
				t.Fatal("exact public tag replay changed", got, err)
			}
			if _, err := cache.TagSHA(upstream, "example", "../main"); err == nil {
				t.Fatal("invalid tag accepted")
			}
		})
	}
}
