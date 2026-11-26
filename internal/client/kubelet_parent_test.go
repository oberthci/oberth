package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMountedReadsResolveTrustedMountParents(t *testing.T) {
	for _, read := range []struct {
		name string
		file func(string) ([]byte, error)
	}{{"private", ReadMountedPrivate}, {"public", ReadPublic}} {
		t.Run(read.name, func(t *testing.T) {
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "ca.crt"), []byte("outside-fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, target := range []string{"..2026_11_25_19_40_09.1", "../outside", outside} {
				checkMountedReadThroughParent(t, read.file, target)
			}
		})
	}
}

func checkMountedReadThroughParent(t *testing.T, read func(string) ([]byte, error), target string) {
	t.Helper()
	path := kubeletMount(t, target)
	if target == "../outside" {
		dir := filepath.Join(filepath.Dir(filepath.Dir(path)), "outside")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("outside-fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(t.TempDir(), "run")
	if err := os.Symlink(filepath.Dir(filepath.Dir(path)), alias); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(alias, filepath.Base(filepath.Dir(path)), filepath.Base(path))
	data, err := read(path)
	if target == "..2026_11_25_19_40_09.1" {
		if err != nil || string(data) != "ca-fixture" {
			t.Fatal("trusted mount parent link was refused", err)
		}
	} else if err == nil {
		t.Fatal("projection escaped through a trusted mount parent")
	}
	clear(data)
}

func TestMountedReadPinsTrustedMountBeforeAliasChanges(t *testing.T) {
	path := kubeletMount(t, "..2026_11_25_19_40_09.1")
	alias := filepath.Join(t.TempDir(), "mount")
	if err := os.Symlink(filepath.Dir(path), alias); err != nil {
		t.Fatal(err)
	}
	mount, err := openKubeletMount(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer mount.Close()
	next := alias + "-next"
	if err := os.Symlink(t.TempDir(), next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, alias); err != nil {
		t.Fatal(err)
	}
	version, err := kubeletVersion(mount, filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	data, err := readKubeletVersion(mount, version, filepath.Base(path))
	if err != nil || string(data) != "ca-fixture" {
		t.Fatal("mount alias change redirected a pinned read", err)
	}
	clear(data)
}
