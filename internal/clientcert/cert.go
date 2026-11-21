package clientcert

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
)

const maxPEMBytes = 64 << 10

// Error is a certificate refusal with diagnostics that contain no key material.
type Error struct {
	Reason string
	Remedy string
}

func (e *Error) Error() string { return e.Reason + "; " + e.Remedy }

func pemBytes(raw []byte, kind string) ([]byte, error) {
	if len(raw) > maxPEMBytes {
		return nil, refusal(fmt.Sprintf("PEM input exceeds the %d-byte (64 KiB) limit", maxPEMBytes), "remove extra data or supply a smaller PEM input")
	}
	raw = bytes.TrimSpace(raw)
	if !bytes.HasPrefix(raw, []byte("-----BEGIN "+kind+"-----")) {
		return nil, refusal("expected one PEM "+kind, "supply exactly one PEM "+kind+" block without extra data or headers")
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != kind || len(block.Headers) > 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, refusal("expected one PEM "+kind, "supply exactly one PEM "+kind+" block without extra data or headers")
	}
	return block.Bytes, nil
}

// ParseCertificate accepts exactly one bounded PEM certificate, with no preamble
// or additional blocks. Parsing alone grants no trust or live identity authority.
func ParseCertificate(raw []byte) (*x509.Certificate, error) {
	der, err := pemBytes(raw, "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, refusal("invalid certificate", "supply a valid X.509 certificate in a CERTIFICATE PEM block")
	}
	return cert, nil
}

// ParseCSR verifies the request signature and permits only Ed25519 or EC P-256.
// Subject and extensions remain untrusted input to the separate issuer policy.
func ParseCSR(raw []byte) (*x509.CertificateRequest, error) {
	der, err := pemBytes(raw, "CERTIFICATE REQUEST")
	if err != nil {
		return nil, err
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil || csr.CheckSignature() != nil {
		return nil, refusal("invalid certificate request signature", "create a new CSR signed with its matching private key")
	}
	switch key := csr.PublicKey.(type) {
	case ed25519.PublicKey:
		return csr, nil
	case *ecdsa.PublicKey:
		if key.Curve == elliptic.P256() {
			return csr, nil
		}
	}
	return nil, refusal("client key must use EC P-256 or Ed25519", "generate an EC P-256 or Ed25519 key and create a new CSR")
}

// Pin returns the lowercase SHA-256 digest of the public key, or empty for an
// absent certificate. It identifies a key; it does not authenticate an identity.
func Pin(cert *x509.Certificate) string {
	if cert == nil || len(cert.RawSubjectPublicKeyInfo) == 0 {
		return ""
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

func refusal(reason, remedy string) *Error { return &Error{Reason: reason, Remedy: remedy} }
