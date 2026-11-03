package gitcache

import "testing"

func TestUpdateRefusesRewriteWhenDefaultBranchUnavailable(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	ready, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	c.env["GIT_DIR"] = ready.Path
	oldSHA := r.commit(t, "advance main", "main advance\n")
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	newSHA := r.featureCommit(t, "rewrite", "divergent commit\n")
	runGit(t, r.work, "push", ready.Path, newSHA+":refs/heads/staging")
	// Lose the server-owned symbolic HEAD while keeping both commit objects.
	runGit(t, ready.Path, "update-ref", "--no-deref", "HEAD", oldSHA)
	if err := c.nativeUpdate(t.Context(), "refs/heads/main", oldSHA, newSHA); err == nil {
		t.Fatal("branch rewrite admitted without identifying the protected default branch")
	}
	if got := remoteRef(t, ready.Path, "refs/heads/main"); got != oldSHA {
		t.Fatalf("main changed after rejection: %s", got)
	}
}
