package installer

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/url"
	"strings"
)

// An old Pod can serve its old name during a URL change. It may only postpone
// readiness after full verification against this installation's pinned CA.
func (e *Engine) previousServerCertificate(cause error, p Profile, previousURL string) bool {
	certificate, previous := previousHostname(cause, previousURL, p.ServerURL)
	if certificate == nil {
		return false
	}
	roots, err := profileCAPool(p)
	if err != nil {
		return false
	}
	options := x509.VerifyOptions{Roots: roots, DNSName: previous, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, Intermediates: unverifiedIntermediates(cause)}
	if e.Now != nil {
		options.CurrentTime = e.Now()
	}
	_, err = certificate.Verify(options)
	return err == nil
}

func previousHostname(cause error, previousURL, currentURL string) (*x509.Certificate, string) {
	previous, oldError := url.Parse(previousURL)
	current, newError := url.Parse(currentURL)
	if oldError != nil || newError != nil {
		return nil, ""
	}
	oldHost, newHost := previous.Hostname(), current.Hostname()
	if previous.Scheme != "https" || current.Scheme != "https" || oldHost == "" || newHost == "" || strings.EqualFold(oldHost, newHost) {
		return nil, ""
	}
	var mismatch x509.HostnameError
	if !errors.As(cause, &mismatch) || mismatch.Certificate == nil || mismatch.Host != newHost {
		return nil, ""
	}
	return mismatch.Certificate, oldHost
}

func unverifiedIntermediates(cause error) *x509.CertPool {
	pool := x509.NewCertPool()
	var verification *tls.CertificateVerificationError
	if errors.As(cause, &verification) {
		for _, cert := range verification.UnverifiedCertificates {
			pool.AddCert(cert)
		}
	}
	return pool
}
