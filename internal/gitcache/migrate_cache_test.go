package gitcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrateToQualifiedLayout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create a flat cache directory.
	flatPath := filepath.Join(root, "oberth.git")
	if err := os.MkdirAll(flatPath, 0o700); err != nil {
		t.Fatal(err)
	}
	// Write a marker file so we can verify the directory content survives.
	if err := os.WriteFile(filepath.Join(flatPath, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cache, err := New(Config{
		Root:     root,
		Upstream: func(repo string) (string, error) { return "/dev/null", nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	repos := map[string]RepoQualification{
		"oberth": {UpstreamName: "github", Org: "oberthci"},
	}
	if err := cache.MigrateToQualifiedLayout(repos); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Flat path should be gone.
	if _, err := os.Stat(flatPath); !os.IsNotExist(err) {
		t.Fatal("flat path should not exist after migration")
	}

	// Qualified path should exist with content.
	qualifiedPath := filepath.Join(root, "github", "oberthci", "oberth.git")
	head, err := os.ReadFile(filepath.Join(qualifiedPath, "HEAD"))
	if err != nil {
		t.Fatalf("read HEAD from qualified path: %v", err)
	}
	if string(head) != "ref: refs/heads/main\n" {
		t.Fatalf("HEAD content = %q, want ref: refs/heads/main", head)
	}
}

func TestMigrateToQualifiedLayoutIdempotent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Pre-create the qualified directory (already migrated).
	qualifiedPath := filepath.Join(root, "github", "oberthci", "oberth.git")
	if err := os.MkdirAll(qualifiedPath, 0o700); err != nil {
		t.Fatal(err)
	}

	cache, err := New(Config{
		Root:     root,
		Upstream: func(repo string) (string, error) { return "/dev/null", nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	repos := map[string]RepoQualification{
		"oberth": {UpstreamName: "github", Org: "oberthci"},
	}
	// Should not error even though flat path doesn't exist.
	if err := cache.MigrateToQualifiedLayout(repos); err != nil {
		t.Fatalf("idempotent migrate: %v", err)
	}
}

// Rewritten with issue #264: the flat-layout assertions this test used to
// pin ("all input forms resolve to <root>/<repo>.git") described the interim
// compromise, which the qualified layout replaces. Without a RepoQualifier,
// fully-parsed segments route the nested path and bare names stay flat;
// with a RepoQualifier every input form of one repository lands on its
// canonical qualified directory.
func TestQualifiedCachePath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	cache, err := New(Config{
		Root:     root,
		Upstream: func(repo string) (string, error) { return "/dev/null", nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		input string
		want  string
	}{
		// Fully specified segments route the qualified layout directly.
		{"github/oberthci/oberth", filepath.Join(root, "github", "oberthci", "oberth.git")},
		// Without a qualifier there is no catalog to resolve the upstream
		// for a 2-segment or bare input, so those stay flat (production
		// wiring always sets the qualifier; internal CLI callers pass the
		// fully-qualified form).
		{"oberthci/oberth", filepath.Join(root, "oberth.git")},
		{"oberth", filepath.Join(root, "oberth.git")},
		{"unknown", filepath.Join(root, "unknown.git")},
	}

	for _, tc := range tests {
		_, got, err := cache.path(tc.input)
		if err != nil {
			t.Errorf("path(%q): %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("path(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestQualifiedCachePathWithQualifier proves the two #264 core properties:
// every input form of ONE repository maps to one canonical directory, and
// same-named repositories under different upstreams map to disjoint
// directories.
func TestQualifiedCachePathWithQualifier(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	qualify := func(input string) (RepoQualification, error) {
		switch input {
		case "terraform", "cloudtaser/terraform", "codeberg/cloudtaser/terraform", "CODEBERG/CLOUDTASER/terraform":
			return RepoQualification{UpstreamName: "codeberg", Org: "cloudtaser"}, nil
		case "oberthci/terraform", "github/oberthci/terraform":
			return RepoQualification{UpstreamName: "github", Org: "oberthci"}, nil
		}
		return RepoQualification{}, fmt.Errorf("repository %q exists under multiple upstreams; qualify the path", input)
	}
	cache, err := New(Config{
		Root:          root,
		Upstream:      func(repo string) (string, error) { return "/dev/null", nil },
		RepoQualifier: qualify,
	})
	if err != nil {
		t.Fatal(err)
	}

	codebergPath := filepath.Join(root, "codeberg", "cloudtaser", "terraform.git")
	githubPath := filepath.Join(root, "github", "oberthci", "terraform.git")

	for _, input := range []string{"terraform", "cloudtaser/terraform", "codeberg/cloudtaser/terraform", "CODEBERG/CLOUDTASER/terraform"} {
		_, got, err := cache.path(input)
		if err != nil {
			t.Fatalf("path(%q): %v", input, err)
		}
		if got != codebergPath {
			t.Fatalf("path(%q) = %q, want canonical %q", input, got, codebergPath)
		}
	}
	for _, input := range []string{"oberthci/terraform", "github/oberthci/terraform"} {
		_, got, err := cache.path(input)
		if err != nil {
			t.Fatalf("path(%q): %v", input, err)
		}
		if got != githubPath {
			t.Fatalf("path(%q) = %q, want %q", input, got, githubPath)
		}
	}
	if codebergPath == githubPath {
		t.Fatal("same-named repositories must map to disjoint cache directories")
	}

	// A qualifier refusal (unknown or ambiguous identity) refuses the path
	// derivation instead of guessing a directory.
	if _, _, err := cache.path("other-repo"); err == nil {
		t.Fatal("qualifier refusal must propagate")
	}
}

func TestListFlatCaches(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create a flat cache and a qualified cache.
	if err := os.MkdirAll(filepath.Join(root, "oberth.git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "github", "oberthci", "terraform.git"), 0o700); err != nil {
		t.Fatal(err)
	}

	cache, err := New(Config{
		Root:     root,
		Upstream: func(repo string) (string, error) { return "/dev/null", nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	flat, err := cache.ListFlatCaches()
	if err != nil {
		t.Fatal(err)
	}
	if len(flat) != 1 || flat[0] != "oberth" {
		t.Fatalf("ListFlatCaches = %v, want [oberth]", flat)
	}
}

// TestReservationIdentityAndPathIsolation proves the durable receive outbox
// cannot alias across same-named repositories (issue #264): the reservation
// identity derives from the resolved cache path, filenames encode it with a
// separator the segment charset forbids, and legacy bare-name files keep
// round-tripping.
func TestReservationIdentityAndPathIsolation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cache, err := New(Config{
		Root:     root,
		Upstream: func(repo string) (string, error) { return "/dev/null", nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	githubIdentity := cache.reservationIdentity("terraform", filepath.Join(root, "github", "oberthci", "terraform.git"))
	codebergIdentity := cache.reservationIdentity("terraform", filepath.Join(root, "codeberg", "cloudtaser", "terraform.git"))
	flatIdentity := cache.reservationIdentity("terraform", filepath.Join(root, "terraform.git"))

	if githubIdentity != "github/oberthci/terraform" || codebergIdentity != "codeberg/cloudtaser/terraform" {
		t.Fatalf("identities = %q, %q", githubIdentity, codebergIdentity)
	}
	if flatIdentity != "terraform" {
		t.Fatalf("flat identity = %q, want bare name for legacy layout", flatIdentity)
	}

	githubFile, err := cache.reservationPath(githubIdentity)
	if err != nil {
		t.Fatal(err)
	}
	codebergFile, err := cache.reservationPath(codebergIdentity)
	if err != nil {
		t.Fatal(err)
	}
	flatFile, err := cache.reservationPath(flatIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if githubFile == codebergFile || githubFile == flatFile || codebergFile == flatFile {
		t.Fatalf("reservation files must be disjoint: %q %q %q", githubFile, codebergFile, flatFile)
	}
	if filepath.Base(githubFile) != "github@oberthci@terraform.json" {
		t.Fatalf("qualified reservation filename = %q", filepath.Base(githubFile))
	}
	if filepath.Base(flatFile) != "terraform.json" {
		t.Fatalf("legacy reservation filename = %q", filepath.Base(flatFile))
	}

	// Round-trip: a qualified reservation written under its identity is
	// listed by PendingReceives with the identity restored from the
	// filename, alongside a legacy bare-name reservation.
	for _, reservation := range []receiveReservation{
		{Version: receiveReservationVersion, ID: "11111111111111111111111111111111", Repo: githubIdentity, Actor: "a@h", State: receiveReady},
		{Version: receiveReservationVersion, ID: "22222222222222222222222222222222", Repo: "legacy-repo", Actor: "a@h", State: receiveReady},
	} {
		if err := cache.writeReservation(reservation); err != nil {
			t.Fatalf("write reservation %s: %v", reservation.Repo, err)
		}
	}
	pending, err := cache.PendingReceives()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d, want 2", len(pending))
	}
	if pending[0].Repo != githubIdentity || pending[1].Repo != "legacy-repo" {
		t.Fatalf("pending identities = %q, %q", pending[0].Repo, pending[1].Repo)
	}
}

// TestDefaultBranchFromSymrefAndEmptyBootstrap pins the empty-upstream
// bootstrap decision (issue #264 rollout): a refless advertisement falls
// back to "main"; an advertisement WITH refs but no symbolic HEAD stays a
// hard error; a symbolic HEAD wins outright. The pure helper is tested with
// the exact shapes GitHub's SSH transport produced for the empty
// oberthci/terraform repository.
func TestDefaultBranchFromSymrefAndEmptyBootstrap(t *testing.T) {
	t.Parallel()

	branch, err := defaultBranchFromSymref("ref: refs/heads/trunk\tHEAD\nabc123\tHEAD\n")
	if err != nil || branch != "trunk" {
		t.Fatalf("symref parse = %q, %v", branch, err)
	}

	if _, err := defaultBranchFromSymref(""); !errors.Is(err, errNoSymbolicDefaultBranch) {
		t.Fatalf("empty advertisement error = %v", err)
	}
	if _, err := defaultBranchFromSymref("abc123\trefs/heads/one\nabc456\trefs/heads/two\n"); !errors.Is(err, errNoSymbolicDefaultBranch) {
		t.Fatalf("refs-without-symref error = %v", err)
	}

	// End to end against a local empty bare upstream: Ensure succeeds and
	// the cache's HEAD lands on a valid branch (the local transport
	// advertises the unborn symref; servers that do not are covered by the
	// pure-function cases above plus the refless fallback in
	// discoverDefaultBranch).
	root := t.TempDir()
	upstream := filepath.Join(root, "empty-upstream.git")
	runGit(t, "", "init", "--bare", "--initial-branch=main", upstream)
	cache, err := New(Config{
		Root:           filepath.Join(root, "cache"),
		CommandTimeout: 10 * time.Second,
		Upstream:       func(string) (string, error) { return upstream, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := cache.Ensure(context.Background(), "empty-repo")
	if err != nil {
		t.Fatalf("Ensure on empty upstream: %v", err)
	}
	if repository.DefaultBranch == "" {
		t.Fatal("empty upstream must still yield a default branch")
	}
}
