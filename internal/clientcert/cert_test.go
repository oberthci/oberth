package clientcert

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"
	"time"
)

func TestIssuancePolicy(t *testing.T) {
	files, err := GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	ca, err := Load(files["ca.crt"], files["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	badEC, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	badRSA, _ := rsa.GenerateKey(rand.Reader, 2048)
	for _, tc := range []struct {
		name  string
		key   any
		valid bool
	}{{"ed", ed, true}, {"p256", ec, true}, {"p384", badEC, false}, {"rsa", badRSA, false}} {
		t.Run(tc.name, func(t *testing.T) {
			der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "attacker"}, DNSNames: []string{"attacker.example"}}, tc.key)
			if err != nil {
				t.Fatal(err)
			}
			raw := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
			_, cert, err := ca.Issue("agent@host", raw, 0)
			if !tc.valid {
				if err == nil {
					t.Fatal("unsupported key accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cert.Subject.CommonName != "agent@host" || len(cert.DNSNames) != 0 || len(cert.URIs) != 1 || cert.URIs[0].String() != "oberth://uplink/agent@host" || cert.IsCA || len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
				t.Fatal("CSR authority escaped identity policy")
			}
			roots := x509.NewCertPool()
			roots.AddCert(ca.Certificate)
			opts := x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
			if _, err := cert.Verify(opts); err != nil {
				t.Fatal(err)
			}
			opts.KeyUsages = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			if _, err := cert.Verify(opts); err == nil {
				t.Fatal("server use accepted")
			}
			for _, ttl := range []time.Duration{-time.Hour, time.Minute, MaxLifetime + time.Hour} {
				if _, _, err := ca.Issue("agent@host", raw, ttl); err == nil {
					t.Fatal("unbounded lifetime")
				}
			}
			der[len(der)-1] ^= 1
			if _, err := ParseCSR(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})); err == nil {
				t.Fatal("bad signature")
			}
		})
	}
}
