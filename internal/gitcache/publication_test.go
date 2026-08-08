package gitcache

import (
	"context"
	"testing"
)

func TestRemoteRefDistinguishesExactObjectFromMissingRef(t *testing.T) {
	t.Parallel()
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	if _, err := cache.Ensure(context.Background(), "example"); err != nil {
		t.Fatal(err)
	}
	sha, exists, err := cache.RemoteRef(context.Background(), "example", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if !exists || sha != repository.initialSHA {
		t.Fatalf("main ref = %q, %v", sha, exists)
	}
	sha, exists, err = cache.RemoteRef(context.Background(), "example", "refs/heads/missing")
	if err != nil {
		t.Fatal(err)
	}
	if exists || sha != "" {
		t.Fatalf("missing ref = %q, %v", sha, exists)
	}
	if _, _, err := cache.RemoteRef(context.Background(), "example", "refs/notes/not-allowed"); err == nil {
		t.Fatal("unsupported remote ref unexpectedly accepted")
	}
}
