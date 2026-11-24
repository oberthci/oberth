package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStageSweepAllowsAnOwnerToFinishAfterEnumeration(t *testing.T) {
	parent := t.TempDir()
	stage, cleanup, err := newStage(parent)
	requireOK(t, err)
	entries, err := os.ReadDir(parent)
	if err != nil {
		_ = cleanup()
		t.Fatal(err)
	}
	requireOK(t, cleanup())
	if len(entries) != 1 || entries[0].Name() != filepath.Base(stage) {
		t.Fatal("stage was not enumerated", entries)
	}
	if err := sweepStagingEntries(parent, entries); err != nil {
		t.Fatal("completed stage cleanup broke a concurrent sweep", err)
	}
}
