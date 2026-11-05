package gitcache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportPinnedTreeExactContentBoundsAndNoGitMetadata(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	r.commitFile(t, "attributes", ".gitattributes", "hidden export-ignore\nsubst export-subst\n")
	r.commitFile(t, "hidden", "hidden", "must remain\n")
	sha := r.commitFile(t, "substitution", "subst", "$Format:%H$\n")
	runGit(t, r.work, "tag", "-a", "source-tag", "-m", "annotated tag")
	runGit(t, r.work, "push", "origin", "source-tag")
	tagObject := runGit(t, r.work, "rev-parse", "source-tag")
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "tree")
	if err := c.ExportPinnedTree(t.Context(), "example", sha, dir, 128<<20); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"hidden": "must remain\n", "subst": "$Format:%H$\n"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(body) != want {
			t.Fatalf("%s: %q %v", name, body, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatal("Git metadata exported")
	}
	for _, tc := range []struct {
		name, sha string
		limit     int64
	}{{"small", sha, 1}, {"missing", strings.Repeat("f", 40), 128 << 20}, {"branch", "main", 128 << 20}, {"tag-object", tagObject, 128 << 20}} {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "tree")
			if c.ExportPinnedTree(t.Context(), "example", tc.sha, dest, tc.limit) == nil {
				t.Fatal("invalid export accepted")
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("partial tree retained")
			}
		})
	}
}

func TestExportPinnedTreeRejectsSymlinks(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	if err := os.Symlink("/etc/passwd", filepath.Join(r.work, "link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, r.work, "add", "link")
	runGit(t, r.work, "commit", "-m", "link")
	runGit(t, r.work, "push", "origin", "main")
	sha := runGit(t, r.work, "rev-parse", "HEAD")
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	if err := c.ExportPinnedTree(t.Context(), "example", sha, filepath.Join(t.TempDir(), "tree"), 128<<20); err == nil {
		t.Fatal("symlink exported")
	}
}
