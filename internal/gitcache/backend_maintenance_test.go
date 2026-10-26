package gitcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGoGitCompactionPreservesPinsAndPrunesOnlyAgedObjects(t *testing.T) {
	repository := newTestRepository(t)
	cache, err := New(Config{Root: filepath.Join(t.TempDir(), "cache"), Backend: BackendGoGit, Upstream: func(string) (string, error) { return repository.upstream, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	private := repository.featureCommit(t, "private-retained", "retained\n")
	seedCacheObject(t, repository.work, ready.Path, private)
	old := runGitStdin(t, ready.Path, "aged unreachable", "hash-object", "-w", "--stdin")
	young := runGitStdin(t, ready.Path, "young unreachable", "hash-object", "-w", "--stdin")
	age := time.Now().Add(-maintenanceGrace - time.Hour)
	oldPath := filepath.Join(ready.Path, "objects", old[:2], old[2:])
	if err := os.Chtimes(oldPath, age, age); err != nil {
		t.Fatal(err)
	}
	runGit(t, ready.Path, "repack", "-ad")
	files, err := filepath.Glob(filepath.Join(ready.Path, "objects/pack/pack-*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if err := os.Chtimes(file, age, age); err != nil {
			t.Fatal(err)
		}
	}
	if err := cache.Compact(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	runGit(t, ready.Path, "fsck", "--strict", "--full", "--no-reflogs")
	for _, hash := range []string{repository.initialSHA, private, young} {
		runGit(t, ready.Path, "cat-file", "-e", hash)
	}
	if _, err := cache.capture(t.Context(), ready.Path, "cat-file", "-e", old); err == nil {
		t.Fatal("aged unreachable object survived compaction")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("aged unreachable loose object survived: %v", err)
	}
	stamp := filepath.Join(ready.Path, maintenanceStamp)
	before, err := os.Stat(stamp)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.GC(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(stamp)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("automatic maintenance ignored cooldown")
	}
}

func TestGoGitLocalCloneHasIndependentObjects(t *testing.T) {
	repository := newTestRepository(t)
	cache, err := New(Config{Root: filepath.Join(t.TempDir(), "cache"), Backend: BackendGoGit, Upstream: func(string) (string, error) { return repository.upstream, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := cache.Checkout(t.Context(), "example", repository.initialSHA, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".git/objects/info/alternates")); !os.IsNotExist(err) {
		t.Fatalf("workspace has alternates: %v", err)
	}
	if err := os.RemoveAll(cache.root); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspace, "fsck", "--strict", "--full")
	if got := runGit(t, workspace, "show", "HEAD:file.txt"); got != "initial" {
		t.Fatalf("workspace lost independent content: %q", got)
	}
}
