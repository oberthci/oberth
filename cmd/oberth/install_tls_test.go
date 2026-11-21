package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/installer"
)

func TestInstalledChainUsesP256AndSHA256(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	profile := installComposedProfile(t.Context(), t, f, 30022)
	now := f.clock.Now()
	identity, err := newServerIdentity(f.ca, f.key, []string{"oberth.local"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		chain, err := identity.certificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		assertBrowserChain(t, chain, f, now)
		now = now.Add(16 * time.Hour)
	}
	verifyInstalledClientCertificate(t, profile)

}

func assertBrowserChain(t *testing.T, chain *tls.Certificate, f *composedFixture, now time.Time) {
	t.Helper()
	if len(chain.Certificate) != 2 {
		t.Fatal("served chain omits installation CA")
	}
	for _, der := range chain.Certificate {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok || pub.Curve != elliptic.P256() || cert.SignatureAlgorithm != x509.ECDSAWithSHA256 {
			t.Fatal("served chain requires P-256 keys and SHA-256 signatures")
		}
		if cert.SignatureAlgorithm == x509.PureEd25519 {
			t.Fatal("served chain has an unsupported signature")
		}
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(f.ca) {
		t.Fatal("installation CA is invalid")
	}
	if _, err := chain.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "oberth.local", CurrentTime: now}); err != nil {
		t.Fatal(err)
	}
}

func verifyInstalledClientCertificate(t *testing.T, profile installer.Profile) {
	t.Helper()
	raw, err := os.ReadFile(profile.AdminCert)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := clientcert.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cert.PublicKey.(ed25519.PublicKey); !ok || cert.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Fatal("administrator key or CA signature changed incorrectly")
	}
}

func TestServerRefusesUnsupportedCAAlgorithms(t *testing.T) {
	for _, algorithm := range []string{"edwards", "p384"} {
		t.Run(algorithm, func(t *testing.T) {
			signer := unsupportedCASigner(t, algorithm)
			f := fixtureServer(t)
			cert := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: f.clock.Now(), NotAfter: f.clock.Now().Add(time.Hour)}
			der, err := x509.CreateCertificate(rand.Reader, cert, cert, signer.Public(), signer)
			if err != nil {
				t.Fatal(err)
			}
			key, err := x509.MarshalPKCS8PrivateKey(signer)
			if err != nil {
				t.Fatal(err)
			}
			_, err = newServerIdentity(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), []string{"oberth.local"}, f.clock.Now)
			if err == nil {
				t.Fatal("unsupported CA accepted", algorithm)
			}
		})
	}
}

func unsupportedCASigner(t *testing.T, algorithm string) crypto.Signer {
	t.Helper()
	if algorithm == "edwards" {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
