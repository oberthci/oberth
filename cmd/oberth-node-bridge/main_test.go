package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProtectedCredentialFileRejectsSymlinkAndLooseMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "operator-key")
	if err := os.WriteFile(path, []byte("test-only"), 0600); err != nil {
		t.Fatal(err)
	}
	if body, err := protectedFile(path, 32, true); err != nil || string(body) != "test-only" {
		t.Fatalf("protected operator file rejected: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := protectedFile(link, 32, true); err == nil {
		t.Fatal("private-key symlink accepted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := protectedFile(path, 32, true); err == nil {
		t.Fatal("world-readable private file accepted")
	}
	if _, err := protectedFile(path, 4, false); err == nil {
		t.Fatal("oversize public file accepted")
	}
}
