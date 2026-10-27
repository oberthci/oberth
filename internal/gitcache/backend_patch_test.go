package gitcache

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoGitPatchProvenance(t *testing.T) {
	root := filepath.Join("..", "..", "third_party", "go-git")
	manifest, err := os.ReadFile(filepath.Join(root, "UPSTREAM.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(manifest)), "\n") {
		expected, path, ok := strings.Cut(line, "  ")
		if !ok {
			t.Fatalf("malformed upstream manifest: %q", line)
		}
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if path == "plumbing/format/packfile/patch_delta.go" {
			source := string(body)
			for _, patch := range [][2]string{{"\t\t\t\t\tbasePos = 0\n", ""}, {"defer func() { _ = baseRd.Close() }()", "defer baseRd.Close()"}} {
				if strings.Count(source, patch[0]) != 1 {
					t.Fatalf("reviewed delta patch changed: %q", patch[0])
				}
				source = strings.Replace(source, patch[0], patch[1], 1)
			}
			body = []byte(source)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != expected {
			t.Fatalf("unreviewed go-git change in %s", path)
		}
		files++
	}
	if files < 250 {
		t.Fatalf("incomplete upstream source manifest: %d", files)
	}
}
