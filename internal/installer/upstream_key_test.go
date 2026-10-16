package installer

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func writeTestWorkKey(t *testing.T, passphrase string) string {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(private, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(private, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "work_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckUpstreamPrivateKey(t *testing.T) {
	t.Parallel()

	if err := CheckUpstreamPrivateKey(writeTestWorkKey(t, "")); err != nil {
		t.Fatalf("unencrypted key rejected: %v", err)
	}
	if err := CheckUpstreamPrivateKey(writeTestWorkKey(t, "secret")); !errors.Is(err, errPassphraseProtectedKey) {
		t.Fatalf("passphrase-protected key: got %v, want errPassphraseProtectedKey", err)
	}
	garbage := filepath.Join(t.TempDir(), "not-a-key")
	if err := os.WriteFile(garbage, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckUpstreamPrivateKey(garbage); err == nil {
		t.Fatal("non-key file accepted")
	}
	if err := CheckUpstreamPrivateKey(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
}

// A passphrase-protected --upstream-key-file is rejected by Validate, before
// any cluster mutation, instead of at onboarding after the full install.
func TestValidateRejectsPassphraseProtectedUpstreamKey(t *testing.T) {
	t.Parallel()

	cfg := Config{UpstreamPrivateKeyPath: writeTestWorkKey(t, "secret")}
	err := cfg.Validate()
	if !errors.Is(err, errPassphraseProtectedKey) {
		t.Fatalf("Validate: got %v, want errPassphraseProtectedKey", err)
	}
}
