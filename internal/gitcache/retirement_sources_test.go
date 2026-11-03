package gitcache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetirementSourcesIncludesFreshAndUnpublishedBranchDeclarations(t *testing.T) {
	r := newTestRepository(t)
	if err := os.MkdirAll(filepath.Join(r.work, ".oberth"), 0700); err != nil {
		t.Fatal(err)
	}
	r.commitFile(t, "pipeline", ".oberth/build.yaml", "kind: Workflow\n")
	c := newTestCache(t, r.upstream)
	ready, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, r.work, "checkout", "-b", "unpublished")
	if err := os.WriteFile(filepath.Join(r.work, ".oberth/tests.yaml"), []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, r.work, "add", ".oberth/tests.yaml")
	runGit(t, r.work, "commit", "-m", "legacy")
	sha := runGit(t, r.work, "rev-parse", "HEAD")
	// Publish only to Oberth's cache, not the upstream.
	runGit(t, r.work, "push", ready.Path, sha+":refs/heads/private")
	rows, err := c.RetirementSources(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Path == ".oberth/tests.yaml" && strings.Contains(row.Ref, "private") {
			found = true
		}
	}
	if !found {
		t.Fatalf("unpublished declaration omitted: %+v", rows)
	}
}

func TestRetirementSourcesRefusesSymlinkAndStaleUpstream(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(r.work, ".oberth"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../file.txt", filepath.Join(r.work, ".oberth/build.yaml")); err != nil {
		t.Fatal(err)
	}
	runGit(t, r.work, "add", ".oberth")
	runGit(t, r.work, "commit", "-m", "link")
	runGit(t, r.work, "push", "origin", "main")
	if _, err := c.RetirementSources(t.Context(), "example"); err == nil {
		t.Fatal("symlink proof accepted")
	}
	if err := os.Rename(r.upstream, r.upstream+"-offline"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RetirementSources(t.Context(), "example"); err == nil {
		t.Fatal("stale proof accepted")
	}
}
