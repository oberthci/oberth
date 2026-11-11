package client

import (
	"crypto/tls"
	"errors"
	"net/http"
	"os"
)

// LoadClientCertificate checks private-key permissions before reading it.
func LoadClientCertificate(certPath, keyPath string) (tls.Certificate, error) {
	if certPath == "" || keyPath == "" {
		return tls.Certificate{}, errors.New("both --client-cert and --client-key are required")
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		return tls.Certificate{}, errors.New("cannot inspect client private key")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return tls.Certificate{}, errors.New("client private key must be a regular owner-only file (0600)")
	}
	if err := trustedConfigFile(keyPath); err != nil {
		return tls.Certificate{}, err
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, errors.New("cannot load matching client certificate and private key")
	}
	return pair, nil
}

// WithClientCertificate clones transport configuration and reloads the files
// on each handshake, so an atomic renewal is picked up without restarting.
func WithClientCertificate(transport http.RoundTripper, certPath, keyPath string) (http.RoundTripper, error) {
	if certPath == "" && keyPath == "" {
		return transport, nil
	}
	if _, err := LoadClientCertificate(certPath, keyPath); err != nil {
		return nil, err
	}
	base, ok := transport.(*http.Transport)
	if !ok {
		return nil, errors.New("certificate transport must be an HTTP transport")
	}
	cloned := base.Clone()
	if cloned.TLSClientConfig == nil {
		cloned.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	}
	cloned.TLSClientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		pair, err := LoadClientCertificate(certPath, keyPath)
		return &pair, err
	}
	return cloned, nil
}
