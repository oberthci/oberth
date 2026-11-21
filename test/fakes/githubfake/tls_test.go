package githubfake

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRootValidation(t *testing.T) {
	if _, err := New(Config{Now: fixtureTime}); err == nil {
		t.Fatal("missing root accepted")
	}
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Root: link, Now: fixtureTime}); err == nil {
		t.Fatal("symlink data root accepted")
	}
}

func TestExistingCASignsLeaf(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := fixtureTime()
	template := &x509.Certificate{SerialNumber: big.NewInt(17), Subject: pkix.Name{CommonName: "Oberth fixture CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	caFile, caKey := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, ca, err := ServerTLS(TLSOptions{Now: now, CACertFile: caFile, CAKeyFile: caKey})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	cert, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "127.0.0.1", CurrentTime: now}); err != nil {
		t.Fatal("generated leaf does not use the supplied CA")
	}
}
func TestDownloadClientProfiles(t *testing.T) {
	binary, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl unavailable; supplied CA verification completed")
	}
	f := newFake(t)
	path := "/org/oberth/releases/download/v1/fixture"
	seed(t, f, Seed{Assets: []Asset{{Path: path, Bytes: []byte("approved bytes")}}})
	server, caFile := trustServer(t, f)
	profiles := [][]string{
		{"--fail", "--location", "--no-progress-meter", "--proto", "=https", "--tlsv1.2", "--max-time", "60", "--retry", "2"},
		{"--fail", "--location", "--no-progress-meter", "--proto", "=https", "--tlsv1.2", "--max-time", "90", "--retry", "2"},
		{"--fail", "--silent", "--show-error", "--location", "--proto", "=https", "--tlsv1.2", "--connect-timeout", "10", "--max-time", "120", "--max-filesize", "16777216"},
		{"--fail", "--silent", "--show-error", "--location", "--proto", "=https", "--max-time", "120"},
		{"--connect-timeout", "10", "--max-time", "40", "-fsSL"},
	}
	for _, args := range profiles {
		args = append(args, "--cacert", caFile, server.URL+path)
		out, err := exec.Command(binary, args...).Output()
		if err != nil || string(out) != "approved bytes" {
			t.Fatal("an inventoried curl profile failed")
		}
	}
}

func TestGeneratedCAValidity(t *testing.T) {
	now := fixtureTime()
	cfg, bundle, err := ServerTLS(TLSOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(bundle)
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA || !ca.NotAfter.Equal(now.AddDate(1, 0, 0)) || !ca.NotBefore.Equal(now.Add(-time.Minute)) {
		t.Fatal("CA does not last one year")
	}
	if !cfg.Certificates[0].Leaf.NotAfter.Equal(ca.NotAfter) {
		t.Fatal("leaf outlives CA")
	}
}
func TestTLSConfigurationRefusals(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for _, options := range []TLSOptions{{CACertFile: missing}, {CAKeyFile: missing}, {CACertFile: missing, CAKeyFile: missing}, {Hosts: []string{""}}} {
		options.Now = fixtureTime()
		if _, _, err := ServerTLS(options); err == nil {
			t.Fatal("invalid TLS options accepted")
		}
	}
}

func fixtureTime() time.Time { return time.Date(2026, time.November, 16, 0, 0, 0, 0, time.UTC) }
func TestClockDependenciesAreRequired(t *testing.T) {
	if _, err := New(Config{Root: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "clock is required") {
		t.Fatalf("missing-clock refusal: %v", err)
	}
	if _, _, err := ServerTLS(TLSOptions{}); err == nil || !strings.Contains(err.Error(), "issuance time is required") {
		t.Fatalf("missing-time refusal: %v", err)
	}
}
