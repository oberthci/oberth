package sourceplugin

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/sourcedelivery"
)

const podTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

type Config struct {
	Endpoint, ServerName, TokenFile string
	CAPEM                           []byte
	Now                             func() time.Time
	Wait                            func(context.Context, time.Duration) error
	Jitter                          func(time.Duration) time.Duration
	DialContext                     func(context.Context, string, string) (net.Conn, error)
	Log                             func(string)
}
type Client struct {
	endpoint, tokenFile string
	http                *http.Client
	now                 func() time.Time
	wait                func(context.Context, time.Duration) error
	jitter              func(time.Duration) time.Duration
	log                 func(string)
}

func Environment() Config {
	return Config{Endpoint: os.Getenv("OBERTH_SOURCE_ENDPOINT"), ServerName: os.Getenv("OBERTH_SOURCE_SERVER_NAME"), CAPEM: []byte(os.Getenv("OBERTH_SOURCE_CA_PEM")), TokenFile: podTokenFile}
}
func New(cfg Config) (*Client, error) {
	u, err := origin(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if cfg.ServerName == "" || strings.TrimSpace(cfg.ServerName) != cfg.ServerName || !filepath.IsAbs(cfg.TokenFile) || filepath.Clean(cfg.TokenFile) != cfg.TokenFile || cfg.Now == nil {
		return nil, failure(reasonConfig)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cfg.CAPEM) {
		return nil, failure(reasonConfig)
	}
	dial := cfg.DialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	transport := &http.Transport{Proxy: nil, DisableCompression: true, TLSHandshakeTimeout: 10 * time.Second, MaxResponseHeaderBytes: 64 << 10,
		DialContext:     dial,
		TLSClientConfig: &tls.Config{Time: cfg.Now, MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: cfg.ServerName, NextProtos: []string{"http/1.1"}}}
	u.Path = sourcedelivery.LoadPath
	if cfg.Wait == nil {
		cfg.Wait = wait
	}
	if cfg.Jitter == nil {
		cfg.Jitter = jitter
	}
	return &Client{endpoint: u.String(), tokenFile: cfg.TokenFile, http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, now: cfg.Now, wait: cfg.Wait, jitter: cfg.Jitter, log: cfg.Log}, nil
}
func (c *Client) Close() { c.http.CloseIdleConnections() }
func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}
func jitter(ceiling time.Duration) time.Duration {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(ceiling)+1))
	if err != nil {
		return ceiling
	}
	return time.Duration(n.Int64())
}

func origin(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil {
		return nil, failure(reasonConfig)
	}
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return nil, failure(reasonConfig)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, failure(reasonConfig)
	}
	return u, nil
}
