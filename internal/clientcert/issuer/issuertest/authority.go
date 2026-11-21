// Package issuertest supplies authority fixtures for credential tests.
//
// Spec: docs/plans/c2-service-actions-and-history.md#owned-data-and-public-contract
package issuertest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"time"
)

// GenerateAuthority exercises the generic issuer with an Ed25519 signer.
func GenerateAuthority(random io.Reader, now func() time.Time) (map[string][]byte, error) {
	if random == nil || now == nil {
		return nil, errors.New("authority fixture dependencies are missing")
	}
	_, key, err := ed25519.GenerateKey(random)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return authority(random, now, key)
}

// GenerateInstallationAuthority models the installation's P-256 signing CA.
func GenerateInstallationAuthority(random io.Reader, now func() time.Time) (map[string][]byte, error) {
	if random == nil || now == nil {
		return nil, errors.New("authority fixture dependencies are missing")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), random)
	if err != nil {
		return nil, err
	}
	return authority(random, now, key)
}

func authority(random io.Reader, now func() time.Time, key crypto.Signer) (map[string][]byte, error) {
	serial, err := rand.Int(random, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	at := now()
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Oberth client CA"}, NotBefore: at.Add(-5 * time.Minute), NotAfter: at.Add(10 * 365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(random, cert, cert, key.Public(), key)
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
