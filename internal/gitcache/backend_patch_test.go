package gitcache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoGitPatchProvenance(t *testing.T) {
	if err := verifyGoGitPatchProvenance(filepath.Join("..", "..", "third_party", "go-git")); err != nil {
		t.Fatal(err)
	}
}

// Check both directions: every manifest file has its reviewed bytes, and every
// on-disk file is accounted for. An added source file must not evade the hash
// loop simply because its author omitted it from the manifest. Pin the reviewed
// manifest too, so adding a file and its hash cannot bypass review.
// go.mod/go.sum record the documented security dependency update; other files
// retain upstream hashes, reversing only the decoder patch below.
func verifyGoGitPatchProvenance(root string) error {
	manifest, err := os.ReadFile(filepath.Join(root, "UPSTREAM.sha256"))
	if err != nil {
		return err
	}
	manifestSum := sha256.Sum256(manifest)
	if hex.EncodeToString(manifestSum[:]) != "81d9c9f3cb2bd193f84546a2c6ea55004cdc0704b2857ef55f65a2626cc05c16" {
		return fmt.Errorf("unreviewed go-git upstream manifest")
	}
	listed := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(manifest)), "\n") {
		expected, path, ok := strings.Cut(line, "  ")
		if !ok || !filepath.IsLocal(filepath.FromSlash(path)) || listed[path] {
			return fmt.Errorf("malformed or duplicate upstream manifest entry: %q", line)
		}
		listed[path] = true
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return err
		}
		if path == "plumbing/format/packfile/patch_delta.go" {
			source := string(body)
			closePatch := "defer func() {\n\t\t\tif baseRd != nil {\n\t\t\t\t_ = baseRd.Close()\n\t\t\t}\n\t\t}()"
			for _, patch := range [][2]string{{"\t\t\t\t\tbasePos = 0\n", ""}, {closePatch, "defer baseRd.Close()"}} {
				if strings.Count(source, patch[0]) != 1 {
					return fmt.Errorf("reviewed delta patch changed: %q", patch[0])
				}
				source = strings.Replace(source, patch[0], patch[1], 1)
			}
			body = []byte(source)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != expected {
			return fmt.Errorf("unreviewed go-git change in %s", path)
		}
	}
	if len(listed) < 250 {
		return fmt.Errorf("incomplete upstream source manifest: %d", len(listed))
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !entry.Type().IsRegular() {
			return fmt.Errorf("non-regular go-git file: %s", relative)
		}
		if !listed[relative] && relative != "OBERTH-PATCH.md" && relative != "UPSTREAM.sha256" {
			return fmt.Errorf("unlisted go-git file: %s", relative)
		}
		return nil
	})
}

func TestGoGitPatchProvenanceRejectsExtraFiles(t *testing.T) {
	original := filepath.Join("..", "..", "third_party", "go-git")
	for _, extra := range []string{"unlisted.go", "plumbing/unlisted.asset", ".unlisted"} {
		t.Run(extra, func(t *testing.T) {
			root := t.TempDir()
			if err := os.CopyFS(root, os.DirFS(original)); err != nil {
				t.Fatal(err)
			}
			if err := verifyGoGitPatchProvenance(root); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, filepath.FromSlash(extra))
			if err := os.WriteFile(path, []byte("unlisted fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := verifyGoGitPatchProvenance(root); err == nil || !strings.Contains(err.Error(), "unlisted go-git file: "+extra) {
				t.Fatalf("extra file escaped provenance check: %v", err)
			}
		})
	}
}

func TestGoGitPatchProvenanceRejectsManifestEdits(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(filepath.Join("..", "..", "third_party", "go-git"))); err != nil {
		t.Fatal(err)
	}
	body := []byte("package git\n")
	if err := os.WriteFile(filepath.Join(root, "extra.go"), body, 0600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "UPSTREAM.sha256")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = fmt.Appendf(manifest, "%x  extra.go\n", sha256.Sum256(body))
	if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyGoGitPatchProvenance(root); err == nil || !strings.Contains(err.Error(), "unreviewed go-git upstream manifest") {
		t.Fatalf("new file and matching manifest entry escaped provenance check: %v", err)
	}
}
