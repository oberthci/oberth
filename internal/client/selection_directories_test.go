package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallationSelectionRemovesOnlyCreatedEmptyDirectories(t *testing.T) {
	for _, kind := range []string{"empty", "existing", "other-file", "replacement"} {
		t.Run(kind, func(t *testing.T) { selectionDirectoryLifetime(t, kind) })
	}
}
func selectionDirectoryLifetime(t *testing.T, kind string) {
	t.Helper()
	home := t.TempDir()
	parent := filepath.Join(home, "config")
	base := filepath.Join(parent, "oberth")
	if kind == "existing" {
		if err := os.MkdirAll(base, 0700); err != nil {
			t.Fatal(err)
		}
	}
	selection, err := PrepareProfileSelection(base, "installed")
	if err != nil {
		t.Fatal(err)
	}
	changeSelectionDirectory(t, kind, base)
	before, err := os.Lstat(base)
	if err != nil {
		t.Fatal(err)
	}
	if err = selection.Close(); err != nil {
		t.Fatal(err)
	}
	if kind == "empty" {
		if _, err = os.Lstat(parent); !os.IsNotExist(err) {
			t.Fatal("new empty directory survived refusal", err)
		}
		return
	}
	assertPreservedSelectionDirectory(t, kind, base, before)
}
func TestInstallationSelectionPreservesExistingModes(t *testing.T) {
	for _, mode := range []os.FileMode{0700, 0755} {
		base := t.TempDir()
		if err := os.Chmod(base, mode); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(base)
		if err != nil {
			t.Fatal(err)
		}
		selection, err := PrepareProfileSelection(base, "installed")
		if mode == 0700 {
			if err != nil {
				t.Fatal(err)
			}
			if err = selection.Close(); err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatal("public configuration directory accepted")
		}
		after, err := os.Lstat(base)
		if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != mode {
			t.Fatal("existing directory was removed or chmodded", err)
		}
	}
}

func changeSelectionDirectory(t *testing.T, kind, base string) {
	t.Helper()
	if kind == "other-file" {
		if err := os.WriteFile(filepath.Join(base, "another-writer"), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "replacement" {
		if err := os.Rename(base, base+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(base, 0700); err != nil {
			t.Fatal(err)
		}
	}
}

func assertPreservedSelectionDirectory(t *testing.T, kind, base string, before os.FileInfo) {
	t.Helper()
	after, err := os.Lstat(base)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatal("cleanup changed an existing/replacement directory", err)
	}
	if kind == "other-file" {
		data, err := os.ReadFile(filepath.Join(base, "another-writer"))
		if err != nil || string(data) != "keep" {
			t.Fatal("cleanup removed another writer's file", err)
		}
	}
}

func TestInstallationSelectionFailureRemovesNewBase(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "new-config")
	if _, err := PrepareProfileSelection(filepath.Join(parent, "oberth"), "../invalid"); err == nil {
		t.Fatal("invalid profile name accepted")
	}
	if _, err := os.Lstat(parent); !os.IsNotExist(err) {
		t.Fatal("failed reservation left new directories", err)
	}
}
func TestInstallationSelectionRetainsExistingParent(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(parent)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(parent, "oberth")
	selection, err := PrepareProfileSelection(base, "installed")
	if err != nil {
		t.Fatal(err)
	}
	if err = selection.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(parent)
	if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0755 {
		t.Fatal("existing parent changed", err)
	}
	if _, err = os.Lstat(base); !os.IsNotExist(err) {
		t.Fatal("new empty base survived refusal", err)
	}
}
