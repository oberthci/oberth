package runlog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPurgeRemovesEveryLogSidecarAndKeepsOtherRuns(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range evidenceSuffixes {
		if err := os.WriteFile(filepath.Join(root, "run-1"+suffix), []byte("retained"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sibling := filepath.Join(root, "run-2.log")
	if err := os.WriteFile(sibling, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := store.UsageByRun()
	if err != nil {
		t.Fatal(err)
	}
	removed, err := store.Purge("run-1")
	if err != nil || removed != before["run-1"] {
		t.Fatalf("removed=%d expected=%d err=%v", removed, before["run-1"], err)
	}
	after, err := store.UsageByRun()
	if err != nil || len(after) != 1 || after["run-2"] != 5 {
		t.Fatalf("remaining=%#v err=%v", after, err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeRefusesTraversalAndSymlinkEvidence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.log")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Purge("../outside"); err == nil {
		t.Fatal("purge accepted a traversal")
	}
	if err := os.Symlink(outside, filepath.Join(root, "logs", "link.log")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Purge("link"); err == nil {
		t.Fatal("purge accepted a symlink")
	}
	if content, err := os.ReadFile(outside); err != nil || string(content) != "keep" {
		t.Fatalf("outside evidence=%q %v", content, err)
	}
}
