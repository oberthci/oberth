package issuer

import (
	"crypto/ed25519"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"github.com/oberthci/oberth/internal/clientcert/issuer/issuertest"
	"io"
	"math/big"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/publicerr"
)

func instant() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }
func entropy() io.Reader { return rand.New(rand.NewSource(42)) }
func authorityFor(t *testing.T) (*Authority, map[string][]byte) {
	t.Helper()
	files, err := issuertest.GenerateAuthority(entropy(), instant)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Load(files["ca.crt"], files["ca.key"], entropy(), instant)
	if err != nil {
		t.Fatal(err)
	}
	return a, files
}
func csrFor(t *testing.T) []byte {
	t.Helper()
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	der, err := x509.CreateCertificateRequest(entropy(), &x509.CertificateRequest{Subject: pkix.Name{CommonName: "untrusted", Organization: []string{"untrusted"}}, DNSNames: []string{"untrusted"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}
func TestIssueUsesApprovedIdentityAndInjectedTime(t *testing.T) {
	a, files := authorityFor(t)
	raw, cert, err := a.Issue("approved", csrFor(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := clientcert.ParseCertificate(files["ca.crt"])
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := clientcert.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	assertIssuedIdentity(t, parsed, cert)
	if err := cert.CheckSignatureFrom(ca); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: instant(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
}
func TestIssueIdentityAtBoundAndBeyond(t *testing.T) {
	a, _ := authorityFor(t)
	csr := csrFor(t)
	if _, _, err := a.Issue(strings.Repeat("i", 256), csr, time.Hour); err != nil {
		t.Fatal("N identity refused", err)
	}
	for _, id := range []string{strings.Repeat("i", 257), "", " spaced", "spaced ", "line\nbreak", "line\rbreak", "zero\x00byte"} {
		if _, _, err := a.Issue(id, csr, time.Hour); err == nil {
			t.Fatalf("invalid identity accepted %q", id)
		}
	}
}
func TestIssueLifetimeAtBoundsAndBeyond(t *testing.T) {
	a, _ := authorityFor(t)
	csr := csrFor(t)
	for _, d := range []time.Duration{time.Hour, MaxLifetime} {
		_, cert, err := a.Issue("worker", csr, d)
		if err != nil || !cert.NotAfter.Equal(instant().Add(d)) {
			t.Fatalf("bound %v: %v", d, err)
		}
	}
	for _, d := range []time.Duration{time.Hour - time.Nanosecond, MaxLifetime + time.Nanosecond, -1} {
		if _, _, err := a.Issue("worker", csr, d); err == nil {
			t.Fatalf("outside lifetime %v accepted", d)
		}
	}
}
func TestIssueCsrAtBoundAndBeyond(t *testing.T) {
	a, _ := authorityFor(t)
	csr := csrFor(t)
	n := 64 << 10
	padded := append(append([]byte{}, csr...), []byte(strings.Repeat(" ", n-len(csr)))...)
	if _, _, err := a.Issue("worker", padded, time.Hour); err != nil {
		t.Fatal("N CSR refused", err)
	}
	if _, _, err := a.Issue("worker", append(padded, ' '), time.Hour); err == nil {
		t.Fatal("N+1 CSR accepted")
	}
	for _, raw := range [][]byte{[]byte("bad"), append(append([]byte{}, csr...), csr...)} {
		if _, _, err := a.Issue("worker", raw, time.Hour); err == nil {
			t.Fatal("invalid CSR accepted")
		}
	}
}
func TestAuthorityCoverageAndDependencies(t *testing.T) {
	a, _ := authorityFor(t)
	csr := csrFor(t)
	a.now = func() time.Time { return a.cert.NotAfter.Add(-time.Hour) }
	if _, _, err := a.Issue("worker", csr, time.Hour); err != nil {
		t.Fatal("exact authority lifetime refused", err)
	}
	a.now = func() time.Time { return a.cert.NotAfter.Add(-time.Hour + time.Second) }
	if _, _, err := a.Issue("worker", csr, time.Hour); err == nil {
		t.Fatal("authority expiry ignored")
	}
	a.now = func() time.Time { return a.cert.NotBefore.Add(-time.Second) }
	if _, _, err := a.Issue("worker", csr, time.Hour); err == nil {
		t.Fatal("authority not-before ignored")
	}
	for _, tc := range []struct {
		random io.Reader
		now    func() time.Time
	}{{nil, instant}, {entropy(), nil}} {
		if _, err := issuertest.GenerateAuthority(tc.random, tc.now); err == nil {
			t.Fatal("nil generation dependency")
		}
		if _, err := Load(nil, nil, tc.random, tc.now); err == nil {
			t.Fatal("nil signing dependency")
		}
	}
}

type brokenEntropy struct{}

func (brokenEntropy) Read([]byte) (int, error) { return 0, errors.New("private entropy diagnostic") }
func TestSigningFailuresHaveBoundedPublicReasons(t *testing.T) {
	a, _ := authorityFor(t)
	a.random = brokenEntropy{}
	_, _, err := a.Issue("worker", csrFor(t), time.Hour)
	var public *publicerr.Error
	if !errors.As(err, &public) || public.Code() != publicerr.Unavailable || strings.Contains(err.Error(), "private") {
		t.Fatalf("signing error: %v", err)
	}
}
func TestLoadRejectsInvalidAuthority(t *testing.T) {
	_, files := authorityFor(t)
	if _, err := Load(files["ca.crt"], []byte("bad"), entropy(), instant); err == nil {
		t.Fatal("invalid key accepted")
	}
	for _, tc := range []struct {
		ca    bool
		usage x509.KeyUsage
	}{{false, x509.KeyUsageDigitalSignature}, {true, x509.KeyUsageDigitalSignature}} {
		key := ed25519.NewKeyFromSeed(make([]byte, 32))
		cert := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: tc.ca, BasicConstraintsValid: true, KeyUsage: tc.usage, NotBefore: instant().Add(-time.Hour), NotAfter: instant().Add(time.Hour)}
		der, err := x509.CreateCertificate(entropy(), cert, cert, key.Public(), key)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Load(encode("CERTIFICATE", der), encode("PRIVATE KEY", raw), entropy(), instant); err == nil {
			t.Fatal("non-signing authority accepted")
		}
	}
}

func assertIssuedIdentity(t *testing.T, parsed, cert *x509.Certificate) {
	if parsed.Subject.CommonName != "approved" || len(parsed.Subject.Organization) != 0 || len(parsed.DNSNames) != 0 || len(parsed.URIs) != 1 || parsed.URIs[0].String() != "oberth://uplink/approved" {
		t.Fatalf("untrusted CSR identity copied: %+v", parsed.Subject)
	}
	if !cert.NotAfter.Equal(instant().Add(DefaultLifetime)) || !cert.NotBefore.Equal(instant().Add(-time.Minute)) || len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth || cert.IsCA {
		t.Fatal("issuer constraints/time")
	}
}

func TestIssueRejectsBadSignature(t *testing.T) {
	a, _ := authorityFor(t)
	block, _ := pem.Decode(csrFor(t))
	block.Bytes[len(block.Bytes)-1] ^= 1
	raw := pem.EncodeToMemory(block)
	if _, _, err := a.Issue("worker", raw, time.Hour); err == nil {
		t.Fatal("invalid CSR signature accepted")
	}
}
func TestLoadRejectsAuthorityNotSignedByItself(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	other := ed25519.NewKeyFromSeed([]byte(strings.Repeat("b", 32)))
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "client CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: instant().Add(-time.Hour), NotAfter: instant().Add(time.Hour)}
	der, err := x509.CreateCertificate(entropy(), cert, cert, key.Public(), other)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(encode("CERTIFICATE", der), encode("PRIVATE KEY", raw), entropy(), instant); err == nil {
		t.Fatal("authority accepted without self signature")
	}
}
