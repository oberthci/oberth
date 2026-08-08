package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitSSHCommandPinsIdentityAndHostVerification(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "operator's files")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(directory, "id_ed25519")
	hostsPath := filepath.Join(directory, "known_hosts")
	if err := os.WriteFile(keyPath, []byte("private-key-placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostsPath, []byte("example.invalid ssh-ed25519 AAAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	command, err := GitSSHCommand(keyPath, hostsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"'-F' '/dev/null'",
		"BatchMode=yes",
		"IdentitiesOnly=yes",
		"StrictHostKeyChecking=yes",
		"GlobalKnownHostsFile=/dev/null",
		"UserKnownHostsFile=",
	} {
		if !strings.Contains(command, required) {
			t.Fatalf("command lacks %q: %s", required, command)
		}
	}
	if !strings.Contains(command, `UserKnownHostsFile="`) {
		t.Fatalf("known_hosts path lacks OpenSSH config quoting: %s", command)
	}
	if strings.Contains(command, "operator's files") {
		t.Fatal("path with quote was not shell-escaped")
	}
	if !strings.Contains(command, "operator'\\''s files") {
		t.Fatalf("escaped command = %s", command)
	}
}

func TestGitSSHCommandPathsRejectsOpenSSHExpansionCharacters(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		`/secrets/${HOME}/key`,
		`/secrets/%h/key`,
		`/secrets/"key"`,
		`/secrets/key\name`,
	} {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			if _, err := GitSSHCommandPaths(path, "/secrets/known_hosts"); err == nil {
				t.Fatalf("expanding path %q passed validation", path)
			}
			if _, err := GitSSHCommandPaths("/secrets/key", path); err == nil {
				t.Fatalf("expanding path %q passed known_hosts validation", path)
			}
		})
	}
}

func TestGitSSHCommandRejectsWorldReadablePrivateKey(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	keyPath := filepath.Join(directory, "key")
	hostsPath := filepath.Join(directory, "known_hosts")
	if err := os.WriteFile(keyPath, []byte("key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostsPath, []byte("host key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := GitSSHCommand(keyPath, hostsPath); err == nil {
		t.Fatal("world-readable private key passed validation")
	}
}
