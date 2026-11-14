package app

import (
	"bytes"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestAdminFingerprintsOneBarePublicKey(t *testing.T) {
	t.Parallel()
	public, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := AuthorizedKeyFingerprintReader(bytes.NewReader(ssh.MarshalAuthorizedKey(key)))
	if err != nil || fingerprint != ssh.FingerprintSHA256(key) {
		t.Fatal(fingerprint, err)
	}
}

func TestAdminRejectsMultipleKeysOptionsAndAmbiguousIdentity(t *testing.T) {
	t.Parallel()
	publicKey, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"multiple": append(ssh.MarshalAuthorizedKey(sshKey), ssh.MarshalAuthorizedKey(sshKey)...), "options": append([]byte("no-pty "), ssh.MarshalAuthorizedKey(sshKey)...), "empty": nil, "oversized": bytes.Repeat([]byte("x"), (64<<10)+1)} {
		if _, err := AuthorizedKeyFingerprintReader(bytes.NewReader(body)); err == nil {
			t.Fatal(name + " passed validation")
		}
	}

	for _, identity := range []string{"agent", " agent@host", "agent @host", "agent@host\n"} {
		if err := ValidateUplinkIdentity(identity); err == nil {
			t.Fatalf("identity %q passed validation", identity)
		}
	}
	if err := ValidateUplinkIdentity("agent@host"); err != nil {
		t.Fatal(err)
	}
}
