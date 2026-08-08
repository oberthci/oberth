package app

import (
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestAdminFingerprintsCertificateAndOneBarePublicKey(t *testing.T) {
	t.Parallel()
	publicKey, privateKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "oberth"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(cryptorand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(t.TempDir(), "tls.crt")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := TLSCertificateFingerprint(certPath)
	if err != nil || !strings.HasPrefix(fingerprint, "SHA256:") {
		t.Fatalf("TLS fingerprint = %q, %v", fingerprint, err)
	}

	sshKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "uplink.pub")
	if err := os.WriteFile(keyPath, ssh.MarshalAuthorizedKey(sshKey), 0o600); err != nil {
		t.Fatal(err)
	}
	keyFingerprint, err := AuthorizedKeyFingerprint(keyPath)
	if err != nil || keyFingerprint != ssh.FingerprintSHA256(sshKey) {
		t.Fatalf("key fingerprint = %q, %v", keyFingerprint, err)
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
	directory := t.TempDir()
	for name, body := range map[string][]byte{
		"multiple.pub": append(ssh.MarshalAuthorizedKey(sshKey), ssh.MarshalAuthorizedKey(sshKey)...),
		"options.pub":  append([]byte("no-pty "), ssh.MarshalAuthorizedKey(sshKey)...),
	} {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := AuthorizedKeyFingerprint(path); err == nil {
			t.Fatalf("%s passed validation", name)
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
