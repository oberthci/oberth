package clientcert

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
)

func testKey(seed byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
}
func certificateFixture(t *testing.T) *x509.Certificate {
	t.Helper()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	key := testKey(1)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Oberth client"}, NotBefore: now, NotAfter: now.Add(time.Hour)}
	raw, err := x509.CreateCertificate(bytes.NewReader(make([]byte, 8192)), template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
func TestParseCertificateExactBoundedPEM(t *testing.T) {
	leaf := certificateFixture(t)
	raw := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
	parsed, err := ParseCertificate(raw)
	if err != nil || !bytes.Equal(parsed.Raw, leaf.Raw) {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty": nil, "size": bytes.Repeat([]byte("x"), maxPEMBytes+1),
		"preamble": append([]byte("ignored\n"), raw...), "trailing": append(bytes.Clone(raw), []byte("other")...),
		"multiple": append(bytes.Clone(raw), raw...), "wrong block": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: leaf.Raw}),
		"invalid DER": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("bad")}),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseCertificate(input)
			var typed *Error
			if !errors.As(errors.Join(errors.New("wrap"), err), &typed) {
				t.Fatal("untyped or missing refusal", err)
			}
			checkMessages(t, typed)
		})
	}
	if Pin(nil) != "" || Pin(&x509.Certificate{}) != "" || len(Pin(leaf)) != 64 || Pin(leaf) != Pin(parsed) {
		t.Fatal("invalid pin")
	}
}
func TestCSRSignatureAndKeyPolicy(t *testing.T) {
	raw, err := x509.CreateCertificateRequest(bytes.NewReader(make([]byte, 8192)), &x509.CertificateRequest{Subject: pkix.Name{CommonName: "untrusted"}}, testKey(2))
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: raw})
	csr, err := ParseCSR(encoded)
	if err != nil || csr.Subject.CommonName != "untrusted" {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if _, err := ParseCSR(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: raw})); err == nil {
		t.Fatal("invalid signature accepted")
	}
	for _, curve := range []elliptic.Curve{elliptic.P256(), elliptic.P384()} {
		scalar := make([]byte, (curve.Params().N.BitLen()+7)/8)
		scalar[len(scalar)-1] = 2
		key, err := ecdsa.ParseRawPrivateKey(curve, scalar)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.CreateCertificateRequest(bytes.NewReader(bytes.Repeat([]byte{7}, 8192)), &x509.CertificateRequest{}, key)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ParseCSR(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
		if (err == nil) != (curve == elliptic.P256()) {
			t.Fatal("curve policy", err)
		}
		if err != nil {
			assertRemedy(t, err, "generate an EC P-256 or Ed25519 key")
		}
	}
	for _, input := range [][]byte{nil, bytes.Repeat([]byte("x"), maxPEMBytes+1), append(encoded, encoded...), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: []byte("bad")})} {
		if _, err := ParseCSR(input); err == nil {
			t.Fatal("malformed request accepted")
		} else {
			assertRemedy(t, err, "")
		}
	}
}

func TestRefusalRemediesNameTheRequiredCorrection(t *testing.T) {
	_, err := ParseCertificate(bytes.Repeat([]byte("x"), maxPEMBytes+1))
	var refusal *Error
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, "65536-byte (64 KiB) limit") || !strings.Contains(refusal.Remedy, "smaller") {
		t.Fatal("unnamed size bound", err)
	}
	der, err := x509.CreateCertificateRequest(bytes.NewReader(make([]byte, 8192)), &x509.CertificateRequest{}, testKey(2))
	if err != nil {
		t.Fatal(err)
	}
	der[len(der)-1] ^= 1
	_, err = ParseCSR(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Remedy, "matching private key") {
		t.Fatal("signature remedy", err)
	}
	checkMessages(t, refusal)
}

func assertRemedy(t *testing.T, err error, want string) {
	t.Helper()
	var refusal *Error
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Remedy, want) {
		t.Fatal("incorrect remedy", err)
	}
	checkMessages(t, refusal)
}

func checkMessages(t *testing.T, e *Error) {
	t.Helper()
	for text, limit := range map[string]int{e.Reason: 80, e.Remedy: 120} {
		if err := publicerr.CheckMessage(text, limit); err != nil {
			t.Fatal(err, text)
		}
	}
}
