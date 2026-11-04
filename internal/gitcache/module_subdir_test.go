package gitcache

import (
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/module"
)

func TestModuleSubdirPseudoVersionTagPolicy(t *testing.T) {
	repo := newTestRepositoryWithBranch(t, "main")
	cache := newTestCache(t, repo.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_AUTHOR_DATE", "2026-10-01T07:08:07Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-10-01T07:08:07Z")
	runGit(t, repo.work, "checkout", "-b", "feature")
	runGit(t, repo.work, "commit", "--allow-empty", "-m", "nested feature")
	source := runGit(t, repo.work, "rev-parse", "HEAD")
	seedCacheObject(t, repo.work, ready.Path, source)
	when := time.Date(2026, 10, 1, 7, 8, 7, 0, time.UTC)
	version := module.PseudoVersion("v0", "", when, source[:12])
	check := func(version string, allow, valid bool) {
		t.Helper()
		sha, _, err := cache.ModuleSubdirPseudoVersionSnapshot(t.Context(), "example", "go.example.test/repository/subdir", "subdir", version, allow)
		if valid {
			if err != nil || sha != source {
				t.Fatalf("nested snapshot = %q: %v", sha, err)
			}
		} else if !errors.Is(err, ErrModulePseudoVersionUnavailable) {
			t.Fatalf("nested policy failed to refuse: %q %v", sha, err)
		}
	}
	// Root and sibling tags cannot authorize this module's orphaned commit.
	for _, tag := range []string{"v1.2.3", "sibling/v1.2.3", "subdir-sibling/v1.2.3"} {
		runGit(t, ready.Path, "update-ref", "refs/tags/"+tag, source)
		check(version, true, false)
	}
	runGit(t, ready.Path, "update-ref", "refs/tags/subdir/v1.2.3", source)
	check(version, false, false)
	check(version, true, true)
	derived := module.PseudoVersion("v1", "v1.0.0", when, source[:12])
	for _, tag := range []string{"v1.0.0", "sibling/v1.0.0"} {
		runGit(t, ready.Path, "update-ref", "refs/tags/"+tag, repo.initialSHA)
		check(derived, true, false)
	}
	runGit(t, ready.Path, "update-ref", "refs/tags/subdir/v1.0.0", repo.initialSHA)
	check(derived, true, true)
	// Removing the authorizing tag invalidates the policy decision, while a
	// real public branch or default-branch ancestry retains its usual authority.
	runGit(t, ready.Path, "update-ref", "-d", "refs/tags/subdir/v1.2.3")
	check(derived, true, false)
	runGit(t, ready.Path, "update-ref", "refs/heads/feature", source)
	check(derived, true, true)
	check(derived, false, false)
	runGit(t, ready.Path, "update-ref", "refs/heads/main", source)
	check(derived, false, true)
	check(module.PseudoVersion("v0", "", when, strings.Repeat("f", 12)), true, false)
	for _, subdir := range []string{"", "../subdir", "/subdir", "subdir/../sibling", "subdir\\sibling", "-subdir"} {
		if _, _, err := cache.ModuleSubdirPseudoVersionSnapshot(t.Context(), "example", "go.example.test/repository/subdir", subdir, version, true); !errors.Is(err, ErrModulePseudoVersionUnavailable) {
			t.Fatalf("invalid subdirectory %q was accepted: %v", subdir, err)
		}
	}
}
