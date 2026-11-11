package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testIdentity is a throwaway CA and a localhost leaf it issued, as PEM.
type testIdentity struct {
	ca, leaf, key []byte
	authority     *x509.Certificate
}

func newTestIdentity(t *testing.T, name string) testIdentity {
	t.Helper()
	encode := func(kind string, der []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}) }
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, authority, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	return testIdentity{ca: encode("CERTIFICATE", caDER), leaf: encode("CERTIFICATE", leafDER), key: encode("PRIVATE KEY", keyDER), authority: authority}
}

// The server presents its CA after the leaf, so a client that pins the CA
// by SHA-256 can read it from the handshake before it trusts anything; a CA
// that did not issue the leaf is never presented.
func TestServedCertificatePresentsItsAuthority(t *testing.T) {
	identity := newTestIdentity(t, "Oberth test CA")
	other := newTestIdentity(t, "Another CA")
	directory := t.TempDir()
	write := func(name string, body []byte) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	certPath, keyPath := write("tls.crt", identity.leaf), write("tls.key", identity.key)

	presented := func(served *servedCertificate) []*x509.Certificate {
		t.Helper()
		server := httptest.NewUnstartedServer(http.NotFoundHandler())
		server.TLS = &tls.Config{GetCertificate: served.getCertificate, MinVersion: tls.VersionTLS13}
		server.StartTLS()
		defer server.Close()
		connection, err := tls.Dial("tcp", server.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, ServerName: "localhost"}) //nolint:gosec // G402: the test reads the presented chain; nothing is trusted.
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = connection.Close() }()
		chain := connection.ConnectionState().PeerCertificates
		if len(chain) == 0 || chain[0].Subject.CommonName != "localhost" {
			t.Fatalf("the server did not present the test leaf: %d certificates", len(chain))
		}
		return chain
	}

	served, err := loadServedCertificate(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if chain := presented(served); len(chain) != 1 {
		t.Fatalf("without ca.crt the server presented %d certificates", len(chain))
	}
	write("ca.crt", append(append([]byte{}, other.ca...), identity.ca...))
	served, err = loadServedCertificate(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	chain := presented(served)
	if len(chain) != 2 || !chain[1].Equal(identity.authority) {
		t.Fatalf("presented %d certificates, want the leaf and its CA", len(chain))
	}

	// A renewed leaf keeps presenting the CA, once.
	pair, err := tls.X509KeyPair(append(append([]byte{}, identity.leaf...), identity.ca...), identity.key)
	if err != nil {
		t.Fatal(err)
	}
	presentAuthority(&pair, identity.ca)
	if len(pair.Certificate) != 2 {
		t.Fatalf("a chain that already ends in the CA grew to %d certificates", len(pair.Certificate))
	}
}
