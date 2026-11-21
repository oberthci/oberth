package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"

	"errors"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/secretstore"
)

type serverIdentity struct {
	mu                  sync.Mutex
	now                 func() time.Time
	ca                  tls.Certificate
	names               []string
	leaf                *tls.Certificate
	issued, expires     time.Time
	certPath, keyPath   string
	certBytes, keyBytes []byte
	reload              func() ([]byte, []byte, error)
}

func newServerIdentity(cert, key []byte, names []string, now func() time.Time) (*serverIdentity, error) {
	s := &serverIdentity{now: now, names: names, certPath: "tls.crt", keyPath: "tls.key"}
	if err := s.load(cert, key); err != nil {
		return nil, err
	}
	if _, err := s.certificate(nil); err != nil {
		return nil, err
	}
	return s, nil
}
func mountedIdentity(certPath, keyPath string, names []string, now func() time.Time) (*serverIdentity, error) {
	read := func() ([]byte, []byte, error) {
		cert, err := secretstore.ReadMountedFile(certPath, 64<<10, "files.ca_certificate")
		if err != nil {
			return nil, nil, err
		}
		key, err := secretstore.ReadMountedFile(keyPath, 64<<10, "files.ca_private_key")
		if err != nil {
			return nil, nil, err
		}
		return cert, key, nil
	}
	cert, key, err := read()
	if err != nil {
		return nil, err
	}
	s, err := newServerIdentity(cert, key, names, now)
	if err != nil {
		var p *publicerr.Error
		if errors.As(err, &p) {
			p.Detail = strings.ReplaceAll(strings.ReplaceAll(p.Detail, "tls.crt", "files.ca_certificate"), "tls.key", "files.ca_private_key")
		}
		return nil, err
	}
	s.certPath, s.keyPath, s.reload = "files.ca_certificate", "files.ca_private_key", read
	return s, nil
}
func (s *serverIdentity) load(cert, key []byte) error {
	block, _ := pem.Decode(cert)
	if block == nil {
		return serverProblem("tls", s.certPath+": not a certificate", nil)
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return serverProblem("tls", s.certPath+": not a certificate", err)
	}
	pub, ok := ca.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() || ca.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		return serverProblem("tls", s.certPath+": CA must use P-256 and SHA-256 signatures", nil)
	}
	if !ca.IsCA {
		return serverProblem("tls", s.certPath+": not a CA", nil)
	}
	if ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		return serverProblem("tls", s.certPath+": certificate signing is not allowed", nil)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return serverProblem("tls", s.keyPath+": key does not match or is invalid", err)
	}
	pair.Leaf = ca
	s.ca, s.certBytes, s.keyBytes, s.leaf = pair, bytes.Clone(cert), bytes.Clone(key), nil
	return nil
}
func (s *serverIdentity) refresh() error {
	if s.reload == nil {
		return nil
	}
	cert, key, err := s.reload()
	if err != nil {
		return err
	}
	if bytes.Equal(cert, s.certBytes) && bytes.Equal(key, s.keyBytes) {
		return nil
	}
	return s.load(cert, key)
}
func (s *serverIdentity) certificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refresh(); err != nil {
		return nil, err
	}
	now := s.now()
	if now.Before(s.ca.Leaf.NotBefore) {
		return nil, serverProblem("tls", s.certPath+": not valid before "+s.ca.Leaf.NotBefore.UTC().Format(time.RFC3339), nil)
	}
	if !now.Before(s.ca.Leaf.NotAfter) {
		return nil, serverProblem("tls", s.certPath+": expired at "+s.ca.Leaf.NotAfter.UTC().Format(time.RFC3339), nil)
	}
	if s.leaf != nil && now.Before(s.leaf.Leaf.NotBefore) {
		return nil, serverProblem("tls", s.certPath+": served certificate is not valid yet", nil)
	}
	if s.leaf != nil && now.Before(s.issued.Add(s.expires.Sub(s.issued)*2/3)) {
		return s.leaf, nil
	}
	return s.issue(now)
}
func (s *serverIdentity) issue(now time.Time) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, serverProblem("tls", s.keyPath+": cannot generate server key", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, serverProblem("tls", s.certPath+": cannot generate certificate serial", err)
	}
	expires := minTime(now.Add(24*time.Hour), s.ca.Leaf.NotAfter)
	template := &x509.Certificate{SignatureAlgorithm: x509.ECDSAWithSHA256, SerialNumber: serial, Subject: pkix.Name{CommonName: s.names[0]}, NotBefore: now.Add(-5 * time.Minute), NotAfter: expires, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, name := range s.names {
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, s.ca.Leaf, &key.PublicKey, s.ca.PrivateKey)
	if err != nil {
		return nil, serverProblem("tls", s.certPath+": cannot issue server certificate", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, serverProblem("tls", s.certPath+": cannot read issued certificate", err)
	}
	s.leaf = &tls.Certificate{Certificate: append([][]byte{der}, s.ca.Certificate...), PrivateKey: key, Leaf: leaf}
	s.issued, s.expires = now, expires
	return s.leaf, nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func (s *serverIdentity) valid() bool       { _, err := s.certificate(nil); return err == nil }
func (s *serverIdentity) expiry() time.Time { s.mu.Lock(); defer s.mu.Unlock(); return s.expires }
func (s *serverIdentity) configuration(hello *tls.ClientHelloInfo) (*tls.Config, error) {
	if _, err := s.certificate(hello); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	roots := x509.NewCertPool()
	roots.AddCert(s.ca.Leaf)
	return &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}, Certificates: []tls.Certificate{*s.leaf}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: roots, Time: s.now}, nil
}
