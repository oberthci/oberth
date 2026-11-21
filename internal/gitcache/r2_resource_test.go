package gitcache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefWriterCleanupOnlyRemovesItsNewRecordedLocks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "refs/heads"), 0700); err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(root, "refs/heads/owned.lock")
	foreign := filepath.Join(root, "refs/heads/pushing.lock")
	s := command{dir: root, refWrites: []string{"refs/heads/owned"},
		args: []string{"update-ref",
			"--stdin"},
		input: strings.NewReader("update refs/heads/owned " + strings.Repeat("a",
			40) + "\n")}
	owned := commandResources(s)
	for _, p := range []string{known, foreign} {
		if err := os.WriteFile(p, []byte("lock"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	quarantine := filepath.Join(root, "objects/tmp_objdir-incoming-live")
	if err := os.MkdirAll(quarantine, 0700); err != nil {
		t.Fatal(err)
	}
	if err := owned.clean(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(known); !os.IsNotExist(err) {
		t.Fatal("owned lock not removed", err)
	}
	for _, p := range []string{foreign, quarantine} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal("foreign resource removed", p, err)
		}
	}
	if err := os.WriteFile(known, []byte("pre-existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := commandResources(s).clean(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(known); err != nil {
		t.Fatal("pre-existing lock removed", err)
	}
}
