package client

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/clientcert"
)

func TestRenewalRequiresClientOnlyLeaf(t *testing.T) {
	keys, err := generateKeys("agent")
	if err != nil {
		t.Fatal(err)
	}
	caPEM, key := certificate(t, 102)
	pair, err := tls.X509KeyPair(caPEM, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := clientcert.ParseCertificate(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	now := fake.NewClock().Now()
	for _, scenario := range []struct {
		name string
		leaf x509.Certificate
	}{
		{"client", x509.Certificate{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}},
		{"ca", x509.Certificate{IsCA: true, BasicConstraintsValid: true, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}},
		{"extra-usage", x509.Certificate{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}},
		{"unknown-usage", x509.Certificate{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, UnknownExtKeyUsage: []asn1.ObjectIdentifier{{1, 2, 3, 4}}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			kind, leaf := scenario.name, &scenario.leaf
			leaf.SerialNumber, leaf.NotBefore, leaf.NotAfter = big.NewInt(2), now, now.Add(time.Hour)
			leaf.KeyUsage = x509.KeyUsageDigitalSignature
			der, err := x509.CreateCertificate(rand.Reader, leaf, ca, keys.public, pair.PrivateKey)
			if err != nil {
				t.Fatal(err)
			}
			path := privateFile(t, "admin.crt", []byte("original certificate"))
			replacement := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
			err = InstallRenewed(path, Renewal{Key: keys.key, CA: caPEM, Certificate: replacement, Now: now})
			if (err == nil) != (kind == "client") {
				t.Fatalf("%s renewal: %v", kind, err)
			}
			saved, readErr := ReadPublic(path)
			if readErr != nil || kind != "client" && string(saved) != "original certificate" {
				t.Fatal("refused renewal changed certificate", readErr)
			}
		})
	}
}

func TestRenewalCSRRefusesTrailingKeyData(t *testing.T) {
	keys, err := generateKeys("agent")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct{ name, suffix string }{
		{"junk", "junk"}, {"whitespace", "\n"}, {"second-key", string(keys.key)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			key := append(append([]byte(nil), keys.key...), scenario.suffix...)
			if _, err := RenewalCSR(key); err == nil {
				t.Fatal("trailing key data accepted")
			}
		})
	}
	if _, err := RenewalCSR(keys.key); err != nil {
		t.Fatal("valid retained key refused", err)
	}
}
