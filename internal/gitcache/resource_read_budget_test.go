package gitcache

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResourceHistoryGitReadsCancelWhileRepositoryLockIsHeld(t *testing.T) {
	cache, err := newBackendTestCache(t, Config{Root: filepath.Join(t.TempDir(), "cache"), CommandTimeout: time.Second, Upstream: func(string) (string, error) { return "repo-01", nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, path, err := cache.path("repo-01")
	if err != nil {
		t.Fatal(err)
	}
	operations := map[string]func(context.Context) error{
		"blob": func(ctx context.Context) error {
			_, e := cache.ReadBlob(ctx, "repo-01", strings.Repeat("a", 40), ".oberth/build.yaml", 1024)
			return e
		},
		"optional": func(ctx context.Context) error {
			_, _, e := cache.ReadOptionalBlob(ctx, "repo-01", strings.Repeat("a", 40), ".oberth/build.yaml", 1024)
			return e
		},
		"default": func(ctx context.Context) error {
			_, _, _, e := cache.ReadDefaultBlob(ctx, "repo-01", ".oberth/build.yaml", 1024)
			return e
		},
		"tag": func(ctx context.Context) error { _, e := cache.TagSHA(ctx, "repo-01", "step-abc12345"); return e },
		"reachable": func(ctx context.Context) error {
			_, e := cache.ReachableFromUpstreamDefault(ctx, "repo-01", strings.Repeat("a", 40))
			return e
		},
	}
	for name, read := range operations {
		t.Run(name, func(t *testing.T) {
			lock := cache.repoLock(path)
			lock.Lock()
			defer lock.Unlock()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- read(ctx) }()
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("read outlived its context while queued on lock")
			}
		})
	}
}
