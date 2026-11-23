package installer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/oberthci/oberth/internal/clientcert"
)

// Verify is read-only and checks ordinary TLS name/chain validation plus the CA pin.
// The transport has no ambient proxy and never follows redirects with credentials.
func Verify(ctx context.Context, p Profile) error { return (EndpointVerifier{}).Verify(ctx, p) }

// EndpointVerifier permits an injected dialer and clock for offline TLS contracts.
type EndpointVerifier struct {
	DialContext func(context.Context, string, string) (net.Conn, error)
	Now         func() time.Time
}

func (v EndpointVerifier) Verify(ctx context.Context, p Profile) error {
	config, err := v.tlsConfig(p)
	if err != nil {
		return err
	}
	transport := &http.Transport{DialContext: v.DialContext, TLSClientConfig: config}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		Timeout:       2 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if _, err = evaluateEndpoint(ctx, client, p.ServerURL+"/readyz"); err != nil {
		return err
	}
	body, err := evaluateEndpoint(ctx, client, p.ServerURL+"/api/status")
	if err != nil {
		return err
	}
	return checkStatus(body, p.ServerVersion)
}
func evaluateEndpoint(ctx context.Context, client *http.Client, address string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, problem("tls", err)
	}
	response, err := client.Do(request)
	if err != nil {
		var certificate *tls.CertificateVerificationError
		if errors.As(err, &certificate) {
			return nil, problem("tls", err)
		}
		return nil, problem("connect", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusServiceUnavailable {
		return nil, problem("starting", nil)
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if response.StatusCode == http.StatusOK && errors.Is(err, context.DeadlineExceeded) {
		return nil, problem("connect", err)
	}
	if err != nil || len(b) > 1<<20 || response.StatusCode != http.StatusOK {
		return nil, described("tls", err, fmt.Sprintf("Endpoint %s returned HTTP %d; body limit is 1 MiB.", address, response.StatusCode))
	}
	return b, nil
}

func (v EndpointVerifier) tlsConfig(p Profile) (*tls.Config, error) {
	roots, err := readProfileCA(p, os.ReadFile)
	if err != nil {
		return nil, err
	}
	cert, err := clientcert.ParseCertificate(roots)
	if err != nil || clientcert.Pin(cert) != p.CAPin {
		return nil, problem("tls", err)
	}
	pair, err := tls.LoadX509KeyPair(p.AdminCert, p.AdminKey)
	if err != nil {
		return nil, problem("tls", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	u, err := url.Parse(p.ServerURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" {
		return nil, problem("tls", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Time:         v.Now,
		RootCAs:      pool,
		ServerName:   u.Hostname(),
		Certificates: []tls.Certificate{pair},
	}, nil
}

func checkStatus(raw []byte, expected string) error {
	var status map[string]any
	if err := json.Unmarshal(raw, &status); err != nil {
		return problem("tls", err)
	}
	if status["ready"] != true {
		return problem("starting", nil)
	}
	if status["version"] != expected {
		return described("ready-version", nil, fmt.Sprintf("Server version: %v\nReviewed release: %s", status["version"], expected))
	}
	return nil
}
