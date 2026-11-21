package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/apicontract"
)

type certificateAuth struct {
	api.Authenticator
	mu    sync.Mutex
	calls int
}

func (a *certificateAuth) Certificate(ctx context.Context, cert *x509.Certificate) (apicontract.Caller, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	return a.Authenticator.Certificate(ctx, cert)
}
func (a *certificateAuth) count() int { a.mu.Lock(); defer a.mu.Unlock(); return a.calls }

func clientIdentity(t *testing.T, f *serverFixture, kind string, key *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	caBlock, _ := pem.Decode(f.ca)
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(f.key)
	signer, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(9), Subject: pkix.Name{CommonName: "agent"}, NotBefore: f.clock.Now().Add(-2 * time.Hour), NotAfter: f.clock.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	switch kind {
	case "expired":
		leaf.NotAfter = f.clock.Now().Add(-time.Hour)
	case "server only":
		leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	case "self signed":
		ca = leaf
		signer = key
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
func TestTLSVerifiesClientsBeforeAuthentication(t *testing.T) {
	f := fixtureServer(t)
	var auth *certificateAuth
	original := f.deps.constructors.ServiceNew
	f.deps.constructors.ServiceNew = func(ctx context.Context, c *composition) (Part, error) {
		p, err := original(ctx, c)
		auth = &certificateAuth{Authenticator: c.auth}
		c.auth = auth
		return p, err
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := f.start(ctx)
	sharedKey := clientSigningKey(t)
	for _, kind := range []string{"valid", "self signed", "expired", "server only"} {
		identity := clientIdentity(t, f, kind, sharedKey)
		client := f.client(f.config.Listen.HTTPS)
		// Force the supplied certificate to the TLS layer, including certificates
		// that automatic client selection would omit.
		client.Transport.(*http.Transport).TLSClientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &identity, nil }
		before := auth.count()
		request, err := http.NewRequestWithContext(t.Context(), "GET", "https://oberth.local/api/runs", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		if kind == "valid" {
			if err != nil || response.StatusCode != 200 || auth.count() != before+1 {
				t.Fatalf("signed client refused: %v", err)
			}
		} else if err == nil || auth.count() != before {
			t.Fatalf("%s reached HTTP/authentication: %v", kind, err)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func changedAuthority(t *testing.T, f *serverFixture, change func(*x509.Certificate)) []byte {
	t.Helper()
	block, _ := pem.Decode(f.ca)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(f.key)
	key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	change(cert)
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, cert.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
func TestAuthorityChecksAndLeafUsage(t *testing.T) {
	f := fixtureServer(t)
	for _, tc := range []struct {
		name   string
		change func(*x509.Certificate)
		detail string
	}{
		{"not CA", func(c *x509.Certificate) { c.IsCA = false }, "not a CA"},
		{"cannot sign", func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageDigitalSignature }, "signing"},
		{"expired", func(c *x509.Certificate) { c.NotAfter = f.clock.Now().Add(-time.Nanosecond) }, "expired"},
		{"future", func(c *x509.Certificate) { c.NotBefore = f.clock.Now().Add(time.Second) }, "not valid before"},
	} {
		_, err := newServerIdentity(changedAuthority(t, f, tc.change), f.key, []string{"oberth.local"}, f.clock.Now)
		if err == nil || !strings.Contains(err.Error(), "tls.crt") || !strings.Contains(err.Error(), tc.detail) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	short := changedAuthority(t, f, func(c *x509.Certificate) { c.NotAfter = f.clock.Now().Add(time.Hour) })
	s, err := newServerIdentity(short, f.key, []string{"oberth.local"}, f.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := s.certificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !leaf.Leaf.NotAfter.Equal(f.clock.Now().Add(time.Hour)) || !leaf.Leaf.NotBefore.Equal(f.clock.Now().Add(-5*time.Minute)) {
		t.Fatal("leaf lifetime not clamped/backdated")
	}
	roots := x509.NewCertPool()
	roots.AddCert(s.ca.Leaf)
	if _, err = leaf.Leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: f.clock.Now(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
		t.Fatal("server leaf grants client usage")
	}
	f.clock.Advance(-5 * time.Minute)
	if !s.valid() {
		t.Fatal("leaf refused at NotBefore")
	}
	f.clock.Advance(-time.Nanosecond)
	if s.valid() {
		t.Fatal("leaf accepted before NotBefore")
	}
}
func TestMountedAuthorityReloadsAndNamesService(t *testing.T) {
	f := fixtureServer(t)
	s, _, err := readMounts(f.config, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.certificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"oberth.local", "oberth", "oberth." + f.config.Namespace + ".svc", "oberth." + f.config.Namespace + ".svc.cluster.local"} {
		if err = first.Leaf.VerifyHostname(name); err != nil {
			t.Fatal("service name omitted", err)
		}
	}
	cert, key := authority(t, f.clock.Now())
	writeAuthority(t, filepath.Dir(f.config.Files.CACertificate), cert, key)
	next, err := s.certificate(nil)
	if err != nil || string(first.Certificate[0]) == string(next.Certificate[0]) {
		t.Fatal("authority did not reload")
	}
	config, err := s.configuration(nil)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cert) {
		t.Fatal("bad replacement CA")
	}
	if !config.ClientCAs.Equal(roots) {
		t.Fatal("client authorities stayed stale")
	}
	if err = os.WriteFile(f.config.Files.CACertificate, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if s.valid() {
		t.Fatal("invalid mounted authority retained readiness")
	}
}
func TestOneLeafAtConcurrentReissue(t *testing.T) {
	f := fixtureServer(t)
	s, err := newServerIdentity(f.ca, f.key, []string{"oberth.local"}, f.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := s.certificate(nil)
	f.clock.Advance(16 * time.Hour)
	results := make(chan *tls.Certificate, 100)
	var workers sync.WaitGroup
	for range 100 {
		workers.Go(func() {
			leaf, err := s.certificate(nil)
			if err != nil {
				t.Error(err)
			}
			results <- leaf
		})
	}
	workers.Wait()
	close(results)
	first := <-results
	if first == nil || first == old {
		t.Fatal("leaf not renewed")
	}
	for leaf := range results {
		if leaf != first {
			t.Fatal("more than one leaf issued")
		}
	}
}

func writeAuthority(t *testing.T, dir string, cert, key []byte) {
	t.Helper()
	for name, data := range map[string][]byte{"tls.crt": cert, "tls.key": key} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func clientSigningKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
