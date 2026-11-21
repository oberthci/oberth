package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestServerIdentitiesSurviveVolumeGroupDirectories(t *testing.T) {
	for _, mode := range []os.FileMode{0700, 0770, os.ModeSetgid | 0770} {
		t.Run(mode.String(), func(t *testing.T) {
			a := serverAdministration{&serverOwners{c: &composition{config: serverConfig{DatabaseFile: filepath.Join(t.TempDir(), "state.db")}}}}
			for _, name := range []string{"shared", "dedicated"} {
				checkVolumeIdentity(t, a, name, mode)
			}
			if err := os.MkdirAll(filepath.Dir(a.pinsPath()), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Dir(a.pinsPath()), mode); err != nil {
				t.Fatal(err)
			}
			if err := a.savePins(t.Context(), nil, []byte("trusted pins")); err != nil {
				t.Fatal("volume group directory blocked host pins", err)
			}
		})
	}
}

func checkVolumeIdentity(t *testing.T, a serverAdministration, name string, mode os.FileMode) {
	t.Helper()
	dir := a.identityDirectory(name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	key, _ := composeSSHKey(t, t.TempDir(), "key")
	raw, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "id_ed25519"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	assertServerIdentityUsable(t, a, name)
	if mode.Perm() != 0700 && client.WritePrivate(filepath.Join(dir, "person.key"), raw) == nil {
		t.Fatal("personal profile accepted group access")
	}
}

func assertServerIdentityUsable(t *testing.T, a serverAdministration, name string) {
	t.Helper()
	for range 2 {
		path, err := a.configure(t.Context(), stateport.Upstream{Name: "origin", BaseURL: "ssh://forge.oberth.ci", KeyName: name})
		if err != nil {
			t.Fatal("volume group directory blocked SSH configuration", err)
		}
		if _, err = gitssh.Configure(path, gitssh.Config{Host: "forge.oberth.ci"}); err != nil {
			t.Fatal("server shim cannot use persisted configuration", err)
		}
		if _, err = a.PublicIdentity(t.Context(), name); err != nil {
			t.Fatal("volume group directory blocked persisted key", err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("server state write is not private", err)
		}
	}
}

func TestServerStateOwnershipAndPermissionBoundary(t *testing.T) {
	for _, directory := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "state")
		if directory {
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, []byte("state"), 0600); err != nil {
			t.Fatal(err)
		}
		checkStateOwnership(t, path, directory)
		for _, mode := range []os.FileMode{0600, 0640, 0644, 0660, 0664, 0610} {
			if directory {
				mode |= os.ModeSetgid | 0110
			}
			checkStatePermissions(t, path, mode, directory)
		}
	}
}

func checkStateOwnership(t *testing.T, path string, directory bool) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	info.Sys().(*syscall.Stat_t).Uid++
	if safeServerState(info, directory) {
		t.Fatal("foreign-owned state accepted")
	}
}

func checkStatePermissions(t *testing.T, path string, mode os.FileMode, directory bool) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	want := mode.Perm()&0007 == 0 && (directory || mode.Perm()&0010 == 0)
	if safeServerState(info, directory) != want {
		t.Fatal("permission boundary", directory, mode)
	}
	info.Sys().(*syscall.Stat_t).Gid++
	if safeServerState(info, directory) != (mode.Perm()&0077 == 0) {
		t.Fatal("foreign group boundary", directory, mode)
	}
}

func TestServerStateRefusesUnsafeTargets(t *testing.T) {
	for _, kind := range []string{"public", "link", "directory", "large"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0770); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "state")
			makeUnsafeServerState(t, kind, path)
			if _, err := readServerState(path); err == nil {
				t.Fatal("unsafe state read", kind)
			}
			if kind != "large" && writeServerState(path, []byte("replacement")) == nil {
				t.Fatal("unsafe state overwritten", kind)
			}
		})
	}
}

func makeUnsafeServerState(t *testing.T, kind, path string) {
	t.Helper()
	var err error
	switch kind {
	case "public":
		err = os.WriteFile(path, []byte("state"), 0644)
	case "link":
		err = os.Symlink("missing", path)
	case "directory":
		err = os.Mkdir(path, 0700)
	case "large":
		err = os.WriteFile(path, make([]byte, 64<<10+1), 0600)
	}
	if err != nil {
		t.Fatal(err)
	}
}
