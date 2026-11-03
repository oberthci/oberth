package gitcache

import (
	"path/filepath"
	"strings"
	"testing"
)

func hookSecurityFixture(t *testing.T) (*testRepository, *Cache, Repository) {
	t.Helper()
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	ready, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the hook policy in this process, with real Git plumbing, so
	// coverage includes checks otherwise executed only in hook subprocesses.
	c.env["GIT_DIR"] = ready.Path
	return r, c, ready
}

func TestPreReceiveRejectsDuplicateCommands(t *testing.T) {
	r, c, ready := hookSecurityFixture(t)
	line := zeroObjectID + " " + r.initialSHA + " refs/heads/duplicate\n"
	if err := c.nativePreReceive(t.Context(), strings.NewReader(line)); err != nil {
		t.Fatalf("valid single command refused: %v", err)
	}
	if err := c.nativePreReceive(t.Context(), strings.NewReader(line+line)); err == nil {
		t.Fatal("duplicate ref commands admitted")
	}
	if got := remoteRef(t, ready.Path, "refs/heads/main"); got != r.initialSHA {
		t.Fatalf("default branch changed: %s", got)
	}
}

func TestPreReceiveRequiresReadablePublicSnapshot(t *testing.T) {
	r, c, _ := hookSecurityFixture(t)
	line := zeroObjectID + " " + r.initialSHA + " refs/heads/new\n"
	if err := c.nativePreReceive(t.Context(), strings.NewReader(line)); err != nil {
		t.Fatal(err)
	}
	// Losing the Git executable must not turn a failed snapshot into an
	// empty repository and bypass ref-limit admission.
	c.gitBinary = filepath.Join(t.TempDir(), "missing-git")
	if err := c.nativePreReceive(t.Context(), strings.NewReader(line)); err == nil {
		t.Fatal("receive admitted without a readable public-ref snapshot")
	}
}

func TestDeleteBranchRefusesWhenDefaultBranchUnavailable(t *testing.T) {
	r, c, ready := hookSecurityFixture(t)
	runGit(t, r.work, "push", "origin", r.initialSHA+":refs/heads/feature")
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	runGit(t, ready.Path, "update-ref", "--no-deref", "HEAD", r.initialSHA)
	if err := c.DeleteBranch(t.Context(), "example", "feature"); err == nil {
		t.Fatal("upstream deletion admitted without identifying the default branch")
	}
	for _, path := range []string{r.upstream, ready.Path} {
		if got := remoteRef(t, path, "refs/heads/feature"); got != r.initialSHA {
			t.Fatalf("feature changed in %s: %s", path, got)
		}
	}
}

func TestOptionalPolicyRequiresImmutableObjectID(t *testing.T) {
	r := newTestRepository(t)
	sha := r.commitFile(t, "policy", "policy.yaml", "required\n")
	c := newTestCache(t, r.upstream)
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	if body, exists, err := c.ReadOptionalBlob(t.Context(), "example", sha, "policy.yaml", 4096); err != nil || !exists || string(body) != "required\n" {
		t.Fatalf("immutable policy refused: %q %v %v", body, exists, err)
	}
	body, exists, err := c.ReadOptionalBlob(t.Context(), "example", "refs/heads/main", "policy.yaml", 4096)
	if err == nil || len(body) != 0 || exists {
		t.Fatalf("mutable ref supplied policy: body=%q exists=%v err=%v", body, exists, err)
	}
}

func TestDefaultPolicyRefusesNewlyDisallowedUpstream(t *testing.T) {
	r := newTestRepository(t)
	r.commitFile(t, "policy", "policy.yaml", "required\n")
	c := newTestCache(t, r.upstream)
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	// The cached remote remains reachable; only the current operator policy
	// has changed. Ignoring reconfiguration would silently reuse that remote.
	c.upstream = func(string) (string, error) { return "https://forge.invalid/repo.git", nil }
	sha, body, exists, err := c.ReadDefaultBlob(t.Context(), "example", "policy.yaml", 4096)
	if err == nil || sha != "" || len(body) != 0 || exists {
		t.Fatalf("disallowed upstream supplied policy: sha=%s body=%q exists=%v err=%v", sha, body, exists, err)
	}
}
