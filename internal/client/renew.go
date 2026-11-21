package client

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"github.com/oberthci/oberth/internal/clientcert"
	"time"
)

// RenewalCSR uses the retained private key; a renewal never replaces that key.
func RenewalCSR(key []byte) ([]byte, error) {
	signer, err := parseSigner(key)
	if err != nil {
		return nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, signer)
	if err != nil {
		return nil, failure("keys", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), nil
}
func parseSigner(key []byte) (crypto.Signer, error) {
	block, rest := pem.Decode(key)
	if block == nil || len(rest) != 0 {
		return nil, failure("keys", nil)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, failure("keys", err)
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, failure("keys", nil)
	}
	return signer, nil
}

// Renewal supplies the retained key, pinned CA, new certificate and verification time.
type Renewal struct {
	Key, CA     []byte
	Certificate string
	Now         time.Time
}

// InstallRenewed verifies the replacement before atomically saving it.
func InstallRenewed(path string, renewal Renewal) error {
	key, ca, certificate, now := renewal.Key, renewal.CA, renewal.Certificate, renewal.Now
	signer, err := parseSigner(key)
	if err != nil {
		return err
	}
	cert, err := clientcert.ParseCertificate([]byte(certificate))
	if err != nil {
		return failure("renewal", err)
	}
	expected, _ := x509.MarshalPKIXPublicKey(signer.Public())
	if !bytes.Equal(expected, cert.RawSubjectPublicKeyInfo) || !clientUsage(cert, now) {
		return failure("renewal", nil)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return failure("trust", nil)
	}
	if _, err = cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return failure("renewal", err)
	}
	return writeClientFile(path, []byte(certificate), false)
}
