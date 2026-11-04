package gitcache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	modzip "golang.org/x/mod/zip"
)

func TestModuleRootLicenseBlobSemanticsAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, body                  string
		symlink, present, oversized bool
	}{
		{name: "regular", body: "license\n", present: true},
		{name: "empty", present: true},
		{name: "symlink", body: "../outside", symlink: true, present: true},
		{name: "missing"},
		{name: "oversized", body: strings.Repeat("x", modzip.MaxLICENSE+1), present: true, oversized: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newTestRepository(t)
			if tc.present {
				var err error
				if tc.symlink {
					err = os.Symlink(tc.body, filepath.Join(repo.work, "LICENSE"))
				} else {
					err = os.WriteFile(filepath.Join(repo.work, "LICENSE"), []byte(tc.body), 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				runGit(t, repo.work, "add", "LICENSE")
				runGit(t, repo.work, "commit", "-m", "root license")
				runGit(t, repo.work, "push", "origin", "HEAD:"+repo.defaultBranch)
			}
			source := runGit(t, repo.work, "rev-parse", "HEAD")
			cache := newTestCache(t, repo.upstream)
			if _, err := cache.Ensure(t.Context(), "example"); err != nil {
				t.Fatal(err)
			}
			body, present, err := cache.ModuleRootLicense(t.Context(), "example", source)
			if tc.oversized {
				if !errors.Is(err, ErrModuleLicenseTooLarge) || body != nil || present {
					t.Fatalf("oversized blob accepted: bytes=%d present=%t error=%v", len(body), present, err)
				}
			} else if err != nil || present != tc.present || string(body) != tc.body {
				t.Fatalf("root blob: present=%t bytes=%q error=%v", present, body, err)
			}
			// An absent source is distinct from a pinned revision without a LICENSE.
			if _, _, err := cache.ModuleRootLicense(t.Context(), "example", strings.Repeat("f", 40)); !errors.Is(err, ErrModuleSourceMissing) {
				t.Fatalf("missing pinned source: %v", err)
			}
		})
	}
}

func TestModuleRootLicenseMissingBlobFailsClosed(t *testing.T) {
	repo := newTestRepository(t)
	cache := newTestCache(t, repo.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	blob := runGitStdin(t, ready.Path, "missing-license-storage-fixture\n", "hash-object", "-w", "--stdin")
	tree := runGitStdin(t, ready.Path, "100644 blob "+blob+"\tLICENSE\n", "mktree")
	source := runGit(t, ready.Path, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit-tree", tree, "-m", "storage fixture")
	// Both the fixture and this cache were created by this test. Remove only
	// the loose blob, leaving its committed tree entry to model object damage.
	if err := os.Remove(filepath.Join(ready.Path, "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	if _, present, err := cache.ModuleRootLicense(t.Context(), "example", source); err == nil || present || errors.Is(err, ErrModuleFileMissing) || errors.Is(err, ErrModuleSourceMissing) {
		t.Fatalf("damaged LICENSE became an optional miss: present=%t error=%v", present, err)
	}
}
