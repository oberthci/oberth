package gitcache

import (
	"testing"
	"time"
)

func TestDefaultPolicyRefreshIsFreshByDefault(t *testing.T) {
	r := newTestRepository(t)
	first := r.commitFile(t, "first", "policy.yaml", "first")
	c := newTestCache(t, r.upstream)
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	if got, _, _, err := c.ReadDefaultBlob(t.Context(), "example", "policy.yaml", 100); err != nil || got != first {
		t.Fatal(got, err)
	}
	second := r.commitFile(t, "second", "policy.yaml", "second")
	if got, _, _, err := c.ReadDefaultBlob(t.Context(), "example", "policy.yaml", 100); err != nil || got != second {
		t.Fatalf("fresh default read returned %s instead of %s: %v", got, second, err)
	}
}

func TestPolicyFreshReadRejectsRefreshStartedBeforeRequest(t *testing.T) {
	r := newTestRepository(t)
	r.commitFile(t, "first", "policy.yaml", "first")
	c := newTestCache(t, r.upstream)
	cached, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.ReadDefaultBlob(t.Context(), "example", "policy.yaml", 100); err != nil {
		t.Fatal(err)
	}
	prior, _ := c.policyRefreshes.Load(cached.Path)
	// This request began just before the leader completed, after its fetch began.
	requested := prior.(policyRefresh).completed.Add(-time.Nanosecond)
	second := r.commitFile(t, "second", "policy.yaml", "second")
	lock := c.repoLock(cached.Path)
	if err := lock.LockContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	got, err := c.refreshPolicyLocked(WithFreshPolicy(t.Context()), "example", cached.Path, requested)
	if err != nil || got != second {
		t.Fatalf("fresh caller reused older fetch: got %s want %s: %v", got, second, err)
	}
}
