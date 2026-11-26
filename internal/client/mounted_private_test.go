package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMountedPrivateAcceptsOnlyConfinedKubeletLinks(t *testing.T) {
	path := kubeletMount(t, "..2026_11_25_19_40_09.1")
	data, err := ReadMountedPrivate(path)
	if err != nil || string(data) != "ca-fixture" {
		t.Fatal("mounted Secret was refused", err)
	}
	clear(data)
	for _, target := range []string{"..", "../outside", t.TempDir()} {
		if _, err = ReadMountedPrivate(kubeletMount(t, target)); err == nil {
			t.Fatal("mounted Secret escaped its mount")
		}
	}
	dir := t.TempDir()
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(dir, "share")
	if err = os.WriteFile(private, []byte("share-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err = ReadMountedPrivate(private)
	if err != nil {
		t.Fatal("ordinary private share was refused", err)
	}
	clear(data)
	if err = os.Chmod(private, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadMountedPrivate(private); err == nil {
		t.Fatal("ordinary public file accepted as a private share")
	}
}
