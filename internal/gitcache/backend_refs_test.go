package gitcache

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestBackendRefCASAndPackedDeletion(t *testing.T) {
	for _, backend := range []string{"cli"} {
		t.Run(backend, func(t *testing.T) {
			repository := newTestRepository(t)
			cache, err := New(Config{Root: filepath.Join(t.TempDir(), "cache"), Upstream: func(string) (string, error) { return repository.upstream, nil }})
			if err != nil {
				t.Fatal(err)
			}
			ready, err := cache.Ensure(t.Context(), "example")
			if err != nil {
				t.Fatal(err)
			}
			one := repository.featureCommit(t, "one", "one\n")
			two := repository.featureCommit(t, "two", "two\n")
			seedCacheObject(t, repository.work, ready.Path, one)
			seedCacheObject(t, repository.work, ready.Path, two)
			const ref = "refs/heads/cas"
			if err := cache.updateRef(t.Context(), ready.Path, ref, repository.initialSHA, strings.Repeat("0", 40)); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			results := make(chan error, 2)
			var workers sync.WaitGroup
			for _, sha := range []string{one, two} {
				workers.Go(func() {
					<-start
					results <- cache.updateRef(context.Background(), ready.Path, ref, sha, repository.initialSHA)
				})
			}
			close(start)
			workers.Wait()
			close(results)
			succeeded := 0
			for err := range results {
				if err == nil {
					succeeded++
				}
			}
			if succeeded != 1 {
				t.Fatalf("CAS winners=%d, want one", succeeded)
			}
			winner := runGit(t, ready.Path, "rev-parse", ref)
			runGit(t, ready.Path, "pack-refs", "--all", "--prune")
			if err := cache.deleteRef(t.Context(), ready.Path, ref, repository.initialSHA); err == nil {
				t.Fatal("stale packed-ref deletion succeeded")
			}
			if err := cache.deleteRef(t.Context(), ready.Path, ref, winner); err != nil {
				t.Fatal(err)
			}
			if got := runGitOptional(t, ready.Path, "show-ref", "--verify", ref); got != "" {
				t.Fatalf("deleted packed ref resurrected: %s", got)
			}
		})
	}
}
