package gitcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureSweepsAbandonedQuarantinesButPreservesActiveAndRecent(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	objects := filepath.Join(ready.Path, "objects")
	now := time.Now()
	old := now.Add(-receiveQuarantineMaxAge - time.Minute)
	directories := map[string]string{}
	for _, name := range []string{"legacy", "crashed", "active", "recent"} {
		path := filepath.Join(objects, "oberth-incoming-"+name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		directories[name] = path
	}
	crashed, err := lockReceiveQuarantine(directories["crashed"], false)
	if err != nil {
		t.Fatal(err)
	}
	if err := crashed.Close(); err != nil {
		t.Fatal(err)
	} // process exit releases this lock too
	active, err := lockReceiveQuarantine(directories["active"], false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = active.Close() }()
	for _, name := range []string{"legacy", "crashed", "active"} {
		if err := os.Chtimes(directories[name], old, old); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	symlink := filepath.Join(objects, "oberth-incoming-symlink")
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	for name, path := range directories {
		_, err := os.Stat(path)
		wantGone := name == "legacy" || name == "crashed"
		if (os.IsNotExist(err)) != wantGone {
			t.Fatalf("quarantine %s: %v", name, err)
		}
	}
	if _, err := os.Lstat(symlink); err != nil {
		t.Fatal("symlink was swept", err)
	}
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}
	// A pending receive reservation uses this cached-only Ensure path too.
	lock := cache.repoLock(ready.Path)
	lock.Lock()
	_, err = cache.ensureCachedOnlyLocked(t.Context(), "example", ready.Path)
	lock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directories["active"]); !os.IsNotExist(err) {
		t.Fatalf("released stale quarantine remains: %v", err)
	}
}
