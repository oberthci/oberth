package installer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBackupStageCleanupEndsAfterFirstRemoval(t *testing.T) {
	t.Parallel()
	final := filepath.Join(t.TempDir(), "published.age")
	file, err := os.CreateTemp(filepath.Dir(final), ".oberth-backup-incomplete-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	identity, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("completed archive")); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	name := file.Name()
	stage := name
	if err = os.Link(name, final); err != nil {
		t.Fatal(err)
	}
	if err = removeBackupStage(identity, &stage); err != nil {
		t.Fatal(err)
	}
	// Reusing the name for the same inode must not renew cleanup authority.
	if err = os.Link(final, name); err != nil {
		t.Fatal(err)
	}
	if err = removeBackupStage(identity, &stage); err != nil {
		t.Fatal("cleanup touched a released name", err)
	}
	raw, err := os.ReadFile(name)
	if err != nil || string(raw) != "completed archive" {
		t.Fatal("cleanup removed the new directory entry", err)
	}
}
