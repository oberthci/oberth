package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallationSelectionRefusesIncompleteProfileAndCleansReservation(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	selection, err := PrepareProfileSelection(base, "installed")
	if err != nil {
		t.Fatal(err)
	}
	if err = selection.Commit(); err == nil {
		t.Fatal("incomplete installation became current")
	}
	if err = selection.Close(); err != nil {
		t.Fatal(err)
	}
	if files, err := os.ReadDir(base); err != nil || len(files) != 0 {
		t.Fatal("selection left a partial publication", files, err)
	}
	if err = os.WriteFile(filepath.Join(base, "current"), []byte("a regular file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = PrepareProfileSelection(base, "installed"); err == nil {
		t.Fatal("unsafe selection target was accepted before apply")
	}
}
