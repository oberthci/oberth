package gitcache

import (
	"io"
	"path/filepath"
	"testing"
)

func TestModuleReadsRejectOptionRefsAndInvalidPaths(t *testing.T) {
	r := newTestRepository(t)
	c := newTestCache(t, r.upstream)
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"--output=" + filepath.Join(t.TempDir(), "overwrite"), "HEAD~1", "main:subdir", ""} {
		if _, err := c.ShowFile(t.Context(), "example", ref, "file.txt"); err == nil {
			t.Errorf("ShowFile accepted %q", ref)
		}
		if err := c.ArchiveTar(t.Context(), "example", ref, io.Discard); err == nil {
			t.Errorf("ArchiveTar accepted %q", ref)
		}
		if _, err := c.TagTime(t.Context(), "example", ref); err == nil {
			t.Errorf("TagTime accepted %q", ref)
		}
	}
	if _, err := c.ShowFile(t.Context(), "example", r.initialSHA, "../file.txt"); err == nil {
		t.Error("ShowFile accepted traversal")
	}
}
