//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/client"
)

func TestServerRecursiveGroupPassKeepsGitWorking(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	close(f.allowList)
	cancel, done := f.run(t)
	defer cancel()
	f.waitStarted(t, done)
	a := serverAdministration{f.owners}
	dir := a.identityDirectory("shared")
	key := filepath.Join(dir, "id_ed25519")
	raw, err := os.ReadFile(f.config.Files.ForgeKey)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	if err = os.WriteFile(key, raw, 0600); err != nil {
		t.Fatal(err)
	}
	setRecursiveGroupFiles(t, []string{key, a.pinsPath()})
	if _, err = client.ReadPrivate(key); err == nil {
		t.Fatal("personal reader accepted group-write key")
	}
	if _, err = a.PublicIdentity(t.Context(), "shared"); err != nil {
		t.Fatal("own-group key refused", err)
	}
	result, err := a.ProbeUpstream(t.Context(), "origin", "team/widget")
	if err != nil || result.Pending {
		t.Fatal("Git failed after recursive volume permissions", result.DisplayReason, err)
	}
	assertGroupFilesUnchanged(t, []string{key, a.pinsPath()})
	if err = os.Chmod(key, 0664); err != nil {
		t.Fatal(err)
	}
	if _, err = a.PublicIdentity(t.Context(), "shared"); err == nil {
		t.Fatal("world-readable key accepted")
	}
}

func setRecursiveGroupFiles(t *testing.T, paths []string) {
	t.Helper()
	for _, path := range paths {
		if err := os.Chmod(filepath.Dir(path), os.ModeSetgid|0770); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0660); err != nil {
			t.Fatal(err)
		}
	}
}

func assertGroupFilesUnchanged(t *testing.T, paths []string) {
	t.Helper()
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0660 {
			t.Fatal("read repaired permissions instead of accepting them", err)
		}
	}
}
