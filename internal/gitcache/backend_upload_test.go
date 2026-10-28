package gitcache

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadPackRealClientCloneAndIncrementalNegotiation(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	// More than one negotiation batch, including divergent client-only haves.
	for i := 0; i < 80; i++ {
		runGit(t, repository.work, "commit", "--allow-empty", "-m", fmt.Sprint(i))
	}
	runGit(t, repository.work, "push", "origin", "main")
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	helper := backendServiceCommand(t, cache, UploadPack)
	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, "", "clone", "--no-local", "--upload-pack="+helper, ready.Path, clone)
	// Also exercises the flush-only request after advertisement.
	runGit(t, clone, "fetch", "--upload-pack="+helper, "origin")
	runGit(t, clone, "config", "user.name", "Test")
	runGit(t, clone, "config", "user.email", "test@example.invalid")
	for i := 0; i < 80; i++ {
		runGit(t, clone, "commit", "--allow-empty", "-m", fmt.Sprint("local", i))
	}
	want := repository.commit(t, "remote update", "updated\n")
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, "fetch", "--upload-pack="+helper, "origin")
	if got := runGit(t, clone, "rev-parse", "origin/main"); got != want {
		t.Fatalf("fetch=%s want %s", got, want)
	}
	if got := runGit(t, clone, "show", "origin/main:file.txt"); got != "updated" {
		t.Fatalf("blob=%q", got)
	}
	if listing := runGit(t, "", "ls-remote", "--upload-pack="+helper, ready.Path); strings.Contains(listing, "refs/oberth/") {
		t.Fatalf("private refs leaked: %s", listing)
	}
	runGit(t, clone, "fsck", "--full")
}
