package gitcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestGoServiceDeadlineInterruptsStalledClient(t *testing.T) {
	repository := newTestRepository(t)
	cache, err := New(Config{Root: filepath.Join(t.TempDir(), "cache"), Backend: BackendGoGit, Upstream: func(string) (string, error) { return repository.upstream, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range []Service{UploadPack, ReceivePack} {
		t.Run(string(service), func(t *testing.T) {
			input, writer := io.Pipe()
			defer func() { _ = writer.Close(); _ = input.Close() }()
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- cache.serveLocked(ctx, ready.Path, service, "", input, io.Discard, io.Discard) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("deadline=%v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stalled client retained service after deadline")
			}
		})
	}
}
