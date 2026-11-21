// Package issuer generates and signs client credentials with injected entropy and time.
//
// Spec: docs/plans/c2-service-actions-and-history.md#owned-data-and-public-contract
package issuer

import (
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/clientcert"
)

const (
	DefaultLifetime = 90 * 24 * time.Hour
	MaxLifetime     = 365 * 24 * time.Hour
)

type Authority struct {
	cert   *x509.Certificate
	key    crypto.Signer
	random io.Reader
	now    func() time.Time
}

func Load(cert, key []byte, random io.Reader, now func() time.Time) (*Authority, error) {
	if random == nil || now == nil {
		return nil, refusal(problemDependencies, nil)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, refusal(problemKeyPair, nil)
	}
	leaf, err := clientcert.ParseCertificate(cert)
	if err != nil {
		return nil, err
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok || leaf.CheckSignatureFrom(leaf) != nil {
		return nil, refusal(problemAuthority, nil)
	}
	return &Authority{cert: leaf, key: signer, random: random, now: now}, nil
}
func (a *Authority) Issue(identity string, raw []byte, lifetime time.Duration) ([]byte, *x509.Certificate, error) {
	if invalidIdentity(identity) {
		return nil, nil, refusal(problemIdentity, nil)
	}
	if lifetime == 0 {
		lifetime = DefaultLifetime
	}
	if lifetime < time.Hour || lifetime > MaxLifetime {
		return nil, nil, refusal(problemLifetime, nil)
	}
	csr, err := clientcert.ParseCSR(raw)
	if err != nil {
		return nil, nil, err
	}
	now := a.now()
	expiry := now.Add(lifetime)
	if now.Before(a.cert.NotBefore) || expiry.After(a.cert.NotAfter) {
		return nil, nil, refusal(problemCoverage, nil)
	}
	serial, err := rand.Int(a.random, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, refusal(problemSigning, err)
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: identity}, URIs: []*url.URL{{Scheme: "oberth", Host: "uplink", Path: "/" + identity}}, NotBefore: now.Add(-time.Minute), NotAfter: expiry, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(a.random, cert, a.cert, csr.PublicKey, a.key)
	if err != nil {
		return nil, nil, refusal(problemSigning, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, refusal(problemSigning, err)
	}
	return encode("CERTIFICATE", der), leaf, nil
}
func encode(kind string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
}

func invalidIdentity(v string) bool {
	return v == "" || len(v) > 256 || strings.TrimSpace(v) != v || strings.ContainsAny(v, "\r\n\x00")
}
