package githubfake

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

// TLSOptions selects hostnames, issuance time and optionally a signing CA.
type TLSOptions struct {
	Hosts                 []string
	Now                   time.Time
	CACertFile, CAKeyFile string
}

// ServerTLS returns TLS settings and the public trust bundle for the caller.
func ServerTLS(o TLSOptions) (*tls.Config, []byte, error) {
	if o.Now.IsZero() {
		return nil, nil, errors.New("TLS issuance time is required")
	}
	ca, bundle, err := signingCA(o)
	if err != nil {
		return nil, nil, err
	}
	template := certificate("Oberth test upstream", o.Now)
	template.NotAfter = ca.Leaf.NotAfter
	template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	if len(o.Hosts) == 0 {
		o.Hosts = []string{"localhost", "127.0.0.1", "::1"}
	}
	for _, host := range o.Hosts {
		if host == "" {
			return nil, nil, errors.New("empty TLS hostname")
		}
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	pair, err := signCertificate(template, ca)
	if err != nil {
		return nil, nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}, bundle, nil
}
func signingCA(o TLSOptions) (tls.Certificate, []byte, error) {
	if (o.CACertFile == "") != (o.CAKeyFile == "") {
		return tls.Certificate{}, nil, errors.New("CA certificate and key must be supplied together")
	}
	if o.CACertFile != "" {
		pair, err := tls.LoadX509KeyPair(o.CACertFile, o.CAKeyFile)
		if err != nil {
			return pair, nil, fmt.Errorf("load CA %s and %s: %w", o.CACertFile, o.CAKeyFile, err)
		}
		if !pair.Leaf.IsCA {
			return pair, nil, fmt.Errorf("certificate %s is not a CA", o.CACertFile)
		}
		return pair, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), nil
	}
	template := certificate("Oberth test CA", o.Now)
	template.IsCA, template.BasicConstraintsValid = true, true
	template.KeyUsage |= x509.KeyUsageCertSign
	pair, err := signCertificate(template, tls.Certificate{})
	if err != nil {
		return pair, nil, err
	}
	return pair, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), nil
}
func certificate(name string, now time.Time) *x509.Certificate {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic("certificate entropy unavailable")
	}
	return &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(1, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature}
}
func signCertificate(template *x509.Certificate, ca tls.Certificate) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	if ca.Leaf == nil {
		ca.Leaf, ca.PrivateKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.Leaf, &key.PublicKey, ca.PrivateKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, err
}
