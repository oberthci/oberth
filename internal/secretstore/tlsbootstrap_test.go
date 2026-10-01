package secretstore

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestBootstrapOpenBaoTLSCreatesPrivatePVCBundleAndReusesIt(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "oberth-tls")

	caPEM, err := BootstrapOpenBaoTLS(directory, "openbao")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(caPEM, []byte("PRIVATE KEY")) {
		t.Fatal("bootstrap output disclosed private key material")
	}
	directoryInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o750 {
		t.Fatalf("TLS directory mode = %04o, want 0750", directoryInfo.Mode().Perm())
	}

	wantModes := map[string]os.FileMode{
		"ca.crt":  0o644,
		"ca.key":  0o600,
		"tls.crt": 0o644,
		"tls.key": 0o640,
	}
	before := make(map[string][]byte, len(wantModes))
	for name, wantMode := range wantModes {
		path := filepath.Join(directory, name)
		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		before[name] = contents
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("stat %s: %v", name, statErr)
		}
		if info.Mode().Perm() != wantMode {
			t.Fatalf("%s mode = %04o, want %04o", name, info.Mode().Perm(), wantMode)
		}
	}
	if !bytes.Equal(caPEM, before["ca.crt"]) {
		t.Fatal("bootstrap output is not exactly the public CA certificate")
	}

	ca := parseCertificateForTest(t, before["ca.crt"])
	leaf := parseCertificateForTest(t, before["tls.crt"])
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:   roots,
		DNSName: "openbao.openbao.svc",
		KeyUsages: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}); err != nil {
		t.Fatalf("verify generated server certificate: %v", err)
	}

	secondCA, err := BootstrapOpenBaoTLS(directory, "openbao")
	if err != nil {
		t.Fatalf("reuse bundle: %v", err)
	}
	if !bytes.Equal(secondCA, caPEM) {
		t.Fatal("idempotent bootstrap rotated the CA")
	}
	for name := range wantModes {
		after, readErr := os.ReadFile(filepath.Join(directory, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(after, before[name]) {
			t.Fatalf("idempotent bootstrap changed %s", name)
		}
	}
}

func TestBootstrapOpenBaoTLSRejectsUnsafeExistingMaterial(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "oberth-tls")
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "ca.key"), []byte("not a key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := BootstrapOpenBaoTLS(directory, "openbao"); err == nil {
		t.Fatal("partial or over-permissive existing TLS material must fail closed")
	}
}

func parseCertificateForTest(t *testing.T, encoded []byte) *x509.Certificate {
	t.Helper()
	block, rest := pem.Decode(encoded)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("invalid certificate PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}
