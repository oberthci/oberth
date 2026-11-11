// Package clientcert implements Oberth's dedicated client certificate authority.
package clientcert

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"strings"
	"time"
)

const DefaultLifetime = 90 * 24 * time.Hour
const MaxLifetime = 365 * 24 * time.Hour

type Authority struct {
	Certificate *x509.Certificate
	key         crypto.Signer
}

func GenerateAuthority() (map[string][]byte, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Oberth client CA"}, NotBefore: time.Now().Add(-5 * time.Minute), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		return nil, err
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	return map[string][]byte{"ca.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "ca.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw})}, nil
}

func Load(cert, key []byte) (*Authority, error) {
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, errors.New("invalid client CA key pair")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, errors.New("invalid client CA certificate")
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok || !leaf.IsCA || leaf.KeyUsage&x509.KeyUsageCertSign == 0 || leaf.CheckSignatureFrom(leaf) != nil {
		return nil, errors.New("invalid client CA")
	}
	return &Authority{Certificate: leaf, key: signer}, nil
}

func Pin(cert *x509.Certificate) string {
	digest := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(digest[:])
}

func ParseCSR(raw []byte) (*x509.CertificateRequest, error) {
	if len(raw) > 64<<10 {
		return nil, errors.New("CSR exceeds 64 KiB")
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("expected one PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		return nil, errors.New("invalid CSR signature")
	}
	switch key := csr.PublicKey.(type) {
	case ed25519.PublicKey:
	case *ecdsa.PublicKey:
		if key.Curve != elliptic.P256() {
			return nil, errors.New("client key must use EC P-256 or Ed25519")
		}
	default:
		return nil, errors.New("client key must use EC P-256 or Ed25519")
	}
	return csr, nil
}

func (a *Authority) Issue(identity string, raw []byte, lifetime time.Duration) ([]byte, *x509.Certificate, error) {
	if identity == "" || strings.TrimSpace(identity) != identity || strings.ContainsAny(identity, "\r\n\x00") || len(identity) > 256 {
		return nil, nil, errors.New("invalid client identity")
	}
	if lifetime == 0 {
		lifetime = DefaultLifetime
	}
	if lifetime < time.Hour || lifetime > MaxLifetime {
		return nil, nil, errors.New("client certificate lifetime must be between 1h and 8760h")
	}
	csr, err := ParseCSR(raw)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	expiry := now.Add(lifetime)
	if now.Before(a.Certificate.NotBefore) || expiry.After(a.Certificate.NotAfter) {
		return nil, nil, errors.New("client CA does not cover requested lifetime")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: identity}, URIs: []*url.URL{{Scheme: "oberth", Host: "uplink", Path: "/" + identity}}, NotBefore: now.Add(-time.Minute), NotAfter: expiry, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, a.Certificate, csr.PublicKey, a.key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), cert, nil
}
