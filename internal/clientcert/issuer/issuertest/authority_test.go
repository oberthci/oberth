package issuertest

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestAuthorityFixtureRoles(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }
	for _, installation := range []bool{false, true} {
		generate := GenerateAuthority
		if installation {
			generate = GenerateInstallationAuthority
		}
		files, err := generate(rand.Reader, now)
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(files["ca.crt"])
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if err = cert.CheckSignatureFrom(cert); err != nil {
			t.Fatal(err)
		}
		if installation {
			key, ok := cert.PublicKey.(*ecdsa.PublicKey)
			if !ok || key.Curve != elliptic.P256() || cert.SignatureAlgorithm != x509.ECDSAWithSHA256 {
				t.Fatal("installation fixture is not P-256/SHA-256")
			}
		} else if _, ok := cert.PublicKey.(ed25519.PublicKey); !ok {
			t.Fatal("generic signer coverage changed")
		}
	}
}
