package client

import (
	"os"
	"path/filepath"
	"testing"
)

// kubeletMount lays out a Secret or ConfigMap volume as kubelet writes it:
// a world-writable mount, a version directory, ..data and a per-key link.
func kubeletMount(t *testing.T, version string) string {
	t.Helper()
	mount := filepath.Join(t.TempDir(), "ca")
	for _, dir := range []string{mount, filepath.Join(mount, "..2026_11_25_19_40_09.1")} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(mount, "..2026_11_25_19_40_09.1", "ca.crt"), []byte("ca-fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(version, filepath.Join(mount, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/ca.crt", filepath.Join(mount, "ca.crt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mount, 0777); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(mount, "ca.crt")
}

func TestReadPublicAcceptsKubeletVolumeLayoutInsideTheMount(t *testing.T) {
	t.Parallel()
	data, err := ReadPublic(kubeletMount(t, "..2026_11_25_19_40_09.1"))
	if err != nil || string(data) != "ca-fixture" {
		t.Fatal("a kubelet-mounted CA was refused", err)
	}
	if _, err := ReadPrivate(kubeletMount(t, "..2026_11_25_19_40_09.1")); err == nil {
		t.Fatal("a private read accepted a linked file")
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "ca.crt"), []byte("foreign"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"..", outside, "../" + filepath.Base(outside)} {
		if _, err := ReadPublic(kubeletMount(t, version)); err == nil {
			t.Fatal("a link leaving the mount was accepted", version)
		}
	}
}
