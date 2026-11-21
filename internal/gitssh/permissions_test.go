package gitssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestConfigAndPinsRecheckWritePermissionsAtUse(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"config", "pins"} {
		t.Run(kind, func(t *testing.T) {
			checkConfiguredPermissions(t, kind, key)
		})
	}
}

func checkConfiguredPermissions(t *testing.T, kind string, key ssh.PublicKey) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, kind)
	content := "Host host\n IdentitiesOnly yes\n BatchMode yes\n StrictHostKeyChecking yes\n GlobalKnownHostsFile /dev/null\n IdentityFile /key\n UserKnownHostsFile /pins\n"
	if kind == "pins" {
		content = knownhosts.Line([]string{"host"}, key) + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := useConfiguredFile(kind, path, dir); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0620, 0602, 0666} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if err := useConfiguredFile(kind, path, dir); err == nil {
			t.Fatal("writable file accepted at use", mode)
		}
	}
}

func useConfiguredFile(kind, path, dir string) error {

	if kind == "config" {
		_, err := Configure(path, Config{Host: "host"})
		return err
	}
	_, _, cleanup, err := pins(path, "host:22", dir, readConfined)
	if cleanup != nil {
		cleanup()
	}
	return err
}
