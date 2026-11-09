package gitcache

import (
	"strings"
	"testing"
)

func TestNonDefaultBranchCountExcludesDefaultAndReservedRefs(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := cache.NonDefaultBranchCount(t.Context(), "example"); err != nil || got != 0 {
		t.Fatalf("fresh mirror count=%d err=%v, want 0", got, err)
	}
	// Two pushed-only branches plus a tag and server-owned refs: only the
	// branches count; the default branch never does.
	for _, ref := range []string{"refs/heads/feature/one", "refs/heads/two", "refs/tags/v1", "refs/oberth/park/x", "refs/replace/" + repository.initialSHA} {
		runGit(t, ready.Path, "update-ref", ref, repository.initialSHA)
	}
	if got, err := cache.NonDefaultBranchCount(t.Context(), "example"); err != nil || got != 2 {
		t.Fatalf("count=%d err=%v, want 2", got, err)
	}
	runGit(t, ready.Path, "update-ref", "-d", "refs/replace/"+repository.initialSHA)
	runGit(t, ready.Path, "pack-refs", "--all", "--prune")
	runGit(t, ready.Path, "update-ref", "-d", "refs/heads/two")
	if got, err := cache.NonDefaultBranchCount(t.Context(), "example"); err != nil || got != 1 {
		t.Fatalf("packed count=%d err=%v, want 1", got, err)
	}
}

func TestNonDefaultBranchCountRefusesUncachedRepository(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	if _, err := cache.NonDefaultBranchCount(t.Context(), "example"); err == nil {
		t.Fatal("uncached repository produced a count")
	}
}

func TestCountNonDefaultBranchesParsesHeadMarker(t *testing.T) {
	got, err := countNonDefaultBranches("  refs/heads/a\n* refs/heads/main\n  refs/heads/b\n")
	if err != nil || got != 2 {
		t.Fatalf("count=%d err=%v", got, err)
	}
	if got, err := countNonDefaultBranches(""); err != nil || got != 0 {
		t.Fatalf("empty count=%d err=%v", got, err)
	}
	for _, bad := range []string{"x refs/heads/a\n", "  refs/tags/v1\n", "refs/heads/a\n"} {
		if _, err := countNonDefaultBranches(bad); err == nil || !strings.Contains(err.Error(), "unexpected ref output") {
			t.Fatalf("%q: err=%v", bad, err)
		}
	}
}
