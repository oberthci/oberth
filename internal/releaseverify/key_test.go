package releaseverify

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const keyPath = "internal/releaseverify/release-cosign.pub"

func TestOnePinnedKey(t *testing.T) {
	command := exec.Command("go", "list", "-json", ".")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		EmbedPatterns []string
		EmbedFiles    []string
	}
	if err := json.Unmarshal(output, &pkg); err != nil {
		t.Fatal(err)
	}
	if len(pkg.EmbedPatterns) != 1 || pkg.EmbedPatterns[0] != "release-cosign.pub" {
		t.Fatalf("embed contract: %+v", pkg)
	}
	if len(pkg.EmbedFiles) != 1 || pkg.EmbedFiles[0] != "release-cosign.pub" {
		t.Fatalf("embedded files: %+v", pkg)
	}
	checkKeyLocations(t, "../..")
	checkEmbeddedKey(t)
}
func checkKeyLocations(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "testdata") {
			return filepath.SkipDir
		}
		if !entry.IsDir() && filepath.Ext(path) == ".pub" && filepath.ToSlash(path) != filepath.ToSlash(filepath.Join(root, keyPath)) {
			t.Errorf("additional public key: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func checkEmbeddedKey(t *testing.T) {
	t.Helper()
	expected, err := os.ReadFile("release-cosign.pub")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pinnedKey, expected) {
		t.Fatal("compiled key differs from canonical file")
	}
	key, err := PublicKey()
	if len(bytes.TrimSpace(expected)) == 0 {
		if !errors.Is(err, Key) {
			t.Fatal("empty pin did not refuse", err)
		}
		return
	}
	if err != nil || !bytes.Equal(key, expected) {
		t.Fatal("compiled public key differs from canonical PEM", err)
	}
}
