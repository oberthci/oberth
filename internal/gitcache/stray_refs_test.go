package gitcache

import (
	"strings"
	"testing"
)

type strayRefSeed struct {
	path, initial, tagged, unique, tree string
}

// seedStrayRefs gives the cached repository stray refs of every class plus a
// private Oberth ref and a tag, all local to the mirror.
func seedStrayRefs(t *testing.T, path, initial string) strayRefSeed {
	t.Helper()
	seed := strayRefSeed{path: path, initial: initial}
	seed.tree = runGit(t, path, "rev-parse", initial+"^{tree}")
	commit := func(message string) string {
		return strings.TrimSpace(runGit(t, path, "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
			"commit-tree", seed.tree, "-p", initial, "-m", message))
	}
	seed.tagged, seed.unique = commit("tagged"), commit("unique")
	runGit(t, path, "update-ref", "refs/tags/v1", seed.tagged)
	runGit(t, path, "update-ref", "refs/remotes/upstream/main", initial)
	runGit(t, path, "update-ref", "refs/remotes/upstream/release", seed.tagged)
	runGit(t, path, "update-ref", "refs/remotes/upstream/cx/work", seed.unique)
	runGit(t, path, "update-ref", "refs/misc/tree", seed.tree)
	runGit(t, path, "update-ref", "refs/oberth/test/private", initial)
	// A symbolic ref whose target is the public default branch.
	runGit(t, path, "symbolic-ref", "refs/remotes/upstream/HEAD", "refs/heads/main")
	return seed
}

func TestStrayRefInventoryClassifiesReachability(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	seed := seedStrayRefs(t, ready.Path, repository.initialSHA)
	inventory, err := cache.StrayRefInventory(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	if inventory.DefaultBranch != "main" || inventory.DefaultSHA != repository.initialSHA || inventory.Truncated {
		t.Fatalf("inventory = %+v", inventory)
	}
	type view struct {
		sha, commit, symref  string
		fromDefault, fromTag bool
	}
	want := map[string]view{
		"refs/misc/tree":                {sha: seed.tree},
		"refs/remotes/upstream/HEAD":    {sha: seed.initial, commit: seed.initial, symref: "refs/heads/main", fromDefault: true},
		"refs/remotes/upstream/cx/work": {sha: seed.unique, commit: seed.unique},
		"refs/remotes/upstream/main":    {sha: seed.initial, commit: seed.initial, fromDefault: true},
		"refs/remotes/upstream/release": {sha: seed.tagged, commit: seed.tagged, fromTag: true},
	}
	if len(inventory.Refs) != len(want) {
		t.Fatalf("stray refs = %+v", inventory.Refs)
	}
	for _, ref := range inventory.Refs {
		expected, ok := want[ref.Ref]
		got := view{sha: ref.SHA, commit: ref.Commit, symref: ref.Symref, fromDefault: ref.ReachableFromDefault, fromTag: ref.ReachableFromTag}
		if !ok || got != expected {
			t.Errorf("%s = %+v, want %+v", ref.Ref, got, expected)
		}
	}
}

func TestPruneStrayRefsLeasesEachRefAndNeverFollowsSymrefs(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	seed := seedStrayRefs(t, ready.Path, repository.initialSHA)
	outcomes, err := cache.PruneStrayRefs(t.Context(), "example", []StrayRefHead{
		{Ref: "refs/remotes/upstream/HEAD", SHA: seed.initial},
		{Ref: "refs/remotes/upstream/release", SHA: seed.tagged},
		{Ref: "refs/remotes/upstream/cx/work", SHA: seed.initial},
		{Ref: "refs/remotes/upstream/gone", SHA: seed.initial},
	})
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]string{}
	for _, outcome := range outcomes {
		results[outcome.Ref] = outcome.Result
	}
	if results["refs/remotes/upstream/HEAD"] != PruneDeleted || results["refs/remotes/upstream/release"] != PruneDeleted ||
		results["refs/remotes/upstream/cx/work"] != PruneMoved || results["refs/remotes/upstream/gone"] != PruneMissing {
		t.Fatalf("outcomes = %+v", outcomes)
	}
	refs := runGit(t, ready.Path, "for-each-ref", "--format=%(refname) %(objectname)")
	for _, kept := range []string{
		"refs/heads/main " + seed.initial, "refs/tags/v1 " + seed.tagged, "refs/oberth/test/private " + seed.initial,
		"refs/remotes/upstream/main " + seed.initial, "refs/remotes/upstream/cx/work " + seed.unique, "refs/misc/tree " + seed.tree,
	} {
		if !strings.Contains(refs, kept+"\n") && !strings.HasSuffix(refs, kept) {
			t.Errorf("ref %q was changed:\n%s", kept, refs)
		}
	}
	for _, deleted := range []string{"refs/remotes/upstream/HEAD", "refs/remotes/upstream/release"} {
		if strings.Contains(refs, deleted+" ") {
			t.Errorf("ref %s still exists:\n%s", deleted, refs)
		}
	}
}

func TestPruneStrayRefsRefusesProtectedNamespaces(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	seed := seedStrayRefs(t, ready.Path, repository.initialSHA)
	for _, ref := range []string{"refs/heads/main", "refs/tags/v1", "refs/oberth/test/private", "refs/replace/x", "HEAD", "refs/heads",
		"refs/remotes/upstream/main\ndelete refs/heads/main", "refs/remotes/../heads/main"} {
		if _, err := cache.PruneStrayRefs(t.Context(), "example", []StrayRefHead{{Ref: ref, SHA: seed.initial}}); err == nil {
			t.Errorf("deletion of %q accepted", ref)
		}
	}
	if got := remoteRef(t, ready.Path, "refs/heads/main"); got != seed.initial {
		t.Fatalf("main = %s", got)
	}
	if got := remoteRef(t, ready.Path, "refs/tags/v1"); got != seed.tagged {
		t.Fatalf("v1 = %s", got)
	}
}

// The cache's upstream remote must carry no fetch refspec: with git's
// default one, every refresh and push wrote refs/remotes/upstream/* into the
// mirror, where nothing pruned them and upload-pack advertised them (#1504).
func TestCacheRefreshWritesNoRemoteTrackingRefs(t *testing.T) {
	repository := newTestRepository(t)
	runGit(t, repository.work, "push", repository.upstream, "HEAD:refs/heads/doomed")
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	if got := runGitOptional(t, ready.Path, "config", "--get-all", "remote.upstream.fetch"); got != "" {
		t.Fatalf("new cache fetch refspec = %q", got)
	}
	if got := runGit(t, ready.Path, "for-each-ref", "--format=%(refname)", "refs/remotes/"); got != "" {
		t.Fatalf("new cache remote-tracking refs:\n%s", got)
	}
	// A cache configured by an earlier version keeps the default refspec
	// until the next refresh drops it.
	runGit(t, ready.Path, "config", "remote.upstream.fetch", "+refs/heads/*:refs/remotes/upstream/*")
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	if got := runGitOptional(t, ready.Path, "config", "--get-all", "remote.upstream.fetch"); got != "" {
		t.Fatalf("refreshed cache fetch refspec = %q", got)
	}
	if got := runGit(t, ready.Path, "for-each-ref", "--format=%(refname)", "refs/remotes/"); got != "" {
		t.Fatalf("refreshed cache remote-tracking refs:\n%s", got)
	}
}
