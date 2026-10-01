package gitcache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveRepositoryDeletesCacheDirectory(t *testing.T) {
	t.Parallel()
	cache := newTestCache(t, "unused")
	path := filepath.Join(cache.root, "widget.git")
	if err := os.MkdirAll(filepath.Join(path, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cache.RemoveRepository("widget"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache directory survives removal: %v", err)
	}

	// Idempotent: removing an already-missing directory succeeds, so a
	// retried administrative removal never fails on its own earlier success.
	if err := cache.RemoveRepository("widget"); err != nil {
		t.Fatalf("second removal = %v, want nil", err)
	}
}

func TestRemoveRepositoryRejectsEscapingNames(t *testing.T) {
	t.Parallel()
	cache := newTestCache(t, "unused")
	// A sibling of the cache root stands in for anything outside the root
	// (other PVC state, the SQLite database, host paths). No removal input
	// may ever reach it.
	sibling := filepath.Join(filepath.Dir(cache.root), "victim.git")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "HEAD"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"", ".", "..", "../victim", "a/../../victim", "/../victim", "victim\x00", `victim\..\..`} {
		if err := cache.RemoveRepository(name); err == nil {
			t.Fatalf("RemoveRepository(%q) succeeded, want validation error", name)
		}
	}
	if _, err := os.Stat(filepath.Join(sibling, "HEAD")); err != nil {
		t.Fatalf("sibling outside the cache root must survive traversal attempts: %v", err)
	}
}
