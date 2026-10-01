package client

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
	"os/exec"
	"strings"
	"time"
)

const (
	requestTimeout  = 30 * time.Second
	maxResponseSize = 8 << 20
	maxErrorBody    = 4 << 10
)

type Config struct {
	BaseURL      string
	Token        string
	TokenCommand string
	CACert       string
}

func FromEnv() Config {
	return Config{
		BaseURL:      strings.TrimSpace(os.Getenv("OBERTH_BASE_URL")),
		Token:        strings.TrimSpace(os.Getenv("OBERTH_TOKEN")),
		TokenCommand: strings.TrimSpace(os.Getenv("OBERTH_TOKEN_COMMAND")),
		CACert:       strings.TrimSpace(os.Getenv("OBERTH_CA_CERT")),
	}
}

func (config Config) Configured() bool { return config.BaseURL != "" }

func (config Config) resolveToken(ctx context.Context) (string, error) {
	if config.Token != "" {
		return config.Token, nil
	}
	if config.TokenCommand == "" {
		return "", errors.New("client: set OBERTH_TOKEN, or OBERTH_TOKEN_COMMAND naming a command that prints one")
	}
	command := exec.CommandContext(ctx, "/bin/sh", "-c", config.TokenCommand) // #nosec G204 -- the operator's own command, from their environment.
	var stderr strings.Builder
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("client: OBERTH_TOKEN_COMMAND failed: %s", detail)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("client: OBERTH_TOKEN_COMMAND printed nothing")
	}
	return token, nil
}

type Client struct {
	base  *url.URL
	token string
	http  *http.Client
	// download shares the same transport but carries no whole-request
	// timeout: http.Client.Timeout covers reading the entire body, and an
	// artifact download bounded only by the server's ceiling (256 MiB per
	// run by default) can legitimately outlive the 30 seconds that are
	// right for an API call. Cancellation still works through the context.
	download *http.Client
}

func New(ctx context.Context, config Config) (*Client, error) {
	if config.BaseURL == "" {
		return nil, errors.New("client: set OBERTH_BASE_URL to the server's https address")
	}
	base, err := url.Parse(config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("client: OBERTH_BASE_URL is not a URL: %w", err)
	}
	if base.Scheme != "https" && base.Scheme != "http" {
		return nil, fmt.Errorf("client: OBERTH_BASE_URL must be http or https, got %q", base.Scheme)
	}
	if base.Host == "" {
		return nil, errors.New("client: OBERTH_BASE_URL has no host")
	}
	if base.Scheme == "http" && !isLoopback(base.Hostname()) {
		return nil, errors.New("client: use https:// — the token would be transmitted in cleartext")
	}
	token, err := config.resolveToken(ctx)
	if err != nil {
		return nil, err
	}
	transport, err := newTransport(config.CACert)
	if err != nil {
		return nil, err
	}
	return &Client{
		base:     base,
		token:    token,
		http:     &http.Client{Timeout: requestTimeout, Transport: transport, CheckRedirect: refuseSchemeDowngrade},
		download: &http.Client{Transport: transport, CheckRedirect: refuseSchemeDowngrade},
	}, nil
}

// refuseSchemeDowngrade rejects any redirect that steps from https down to a
// non-https scheme before the request is sent. Go's http.Client re-sends the
// Authorization header on same-host redirects without considering the scheme,
// so a hostile or misconfigured server could otherwise downgrade-redirect and
// cause a cleartext bearer-token re-send. It also preserves the standard
// library's default ten-redirect ceiling, which setting CheckRedirect replaces.
func refuseSchemeDowngrade(request *http.Request, via []*http.Request) error {
	if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && request.URL.Scheme != "https" {
		return fmt.Errorf("%w (redirect target scheme %q)", errRedirectDowngrade, request.URL.Scheme)
	}
	if len(via) >= 10 {
		return errors.New("client: stopped after 10 redirects")
	}
	return nil
}

// isLoopback reports whether host is a loopback address safe for plain HTTP.
// Only 127.0.0.1, localhost, and ::1 qualify; everything else must use TLS
// to protect the bearer token.
func isLoopback(host string) bool {
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

func newTransport(caCert string) (http.RoundTripper, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("client: http.DefaultTransport is not *http.Transport; cannot configure TLS")
	}
	cloned := transport.Clone()
	if cloned.TLSClientConfig == nil {
		cloned.TLSClientConfig = &tls.Config{}
	}
	cloned.TLSClientConfig.MinVersion = tls.VersionTLS13
	if caCert == "" {
		return cloned, nil
	}
	pem, err := os.ReadFile(caCert) // #nosec G304 -- operator-supplied trust anchor path.
	if err != nil {
		return nil, fmt.Errorf("client: read OBERTH_CA_CERT: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("client: OBERTH_CA_CERT %s contains no certificate", caCert)
	}
	cloned.TLSClientConfig.RootCAs = pool
	return cloned, nil
}

// newGetRequest builds an authenticated GET for path. Every request path
// shares it so the Authorization header can never diverge between the JSON
// and the raw-download reads.
func (client *Client) newGetRequest(ctx context.Context, path string, query map[string]string) (*http.Request, error) {
	target := *client.base
	target.Path = strings.TrimRight(target.Path, "/") + path
	if len(query) > 0 {
		values := url.Values{}
		for name, value := range query {
			if value != "" {
				values.Set(name, value)
			}
		}
		target.RawQuery = values.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("client: build request for %s: %w", path, err)
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	return request, nil
}

func (client *Client) Get(ctx context.Context, path string, query map[string]string, into any) error {
	request, err := client.newGetRequest(ctx, path, query)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")

	response, err := client.http.Do(request)
	if err != nil {
		return classifyTransport(path, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return statusError(path, response)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("client: read %s: %w", path, err)
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("client: %s returned a body that is not the expected JSON", path)
	}
	return nil
}

func (client *Client) GetRaw(ctx context.Context, path string, query map[string]string) ([]byte, error) {
	var raw json.RawMessage
	if err := client.Get(ctx, path, query, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// GetTo streams the response body into output without JSON decoding and
// without maxResponseSize: that cap exists to bound JSON decoding in
// memory, but an artifact is served as application/octet-stream and the
// server's default per-run artifact ceiling is 256 MiB, so capping the
// read at 8 MiB silently truncated every larger download (#237). The
// transfer is bounded by the server's own artifact limits and cancelled
// through ctx; nothing is buffered beyond io.Copy's window.
func (client *Client) GetTo(ctx context.Context, path string, query map[string]string, output io.Writer) error {
	request, err := client.newGetRequest(ctx, path, query)
	if err != nil {
		return err
	}
	response, err := client.download.Do(request)
	if err != nil {
		return classifyTransport(path, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return statusError(path, response)
	}
	if _, err := io.Copy(output, response.Body); err != nil {
		return fmt.Errorf("client: read %s: %w", path, err)
	}
	return nil
}

// errRedirectDowngrade marks a refused https-to-http downgrade redirect so
// classifyTransport can surface the reason instead of the generic bucket.
var errRedirectDowngrade = errors.New("client: refusing a redirect from https to a cleartext scheme — the token would be transmitted in cleartext")

func classifyTransport(path string, err error) error {
	if errors.Is(err, errRedirectDowngrade) {
		return fmt.Errorf("%w for %s", errRedirectDowngrade, path)
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return fmt.Errorf("client: the server's certificate does not cover %q; it is issued for %s. "+
			"Reach the server by one of those names, or have the operator add this address to the certificate",
			hostname.Host, strings.Join(certificateNames(hostname.Certificate), ", "))
	}
	var authority x509.UnknownAuthorityError
	if errors.As(err, &authority) {
		return fmt.Errorf("client: the server's certificate is signed by an authority this machine "+
			"does not trust; set OBERTH_CA_CERT to the PEM that signed it (%s)", path)
	}
	if strings.Contains(err.Error(), "x509") || strings.Contains(err.Error(), "certificate") {
		return fmt.Errorf("client: the server's certificate could not be verified for %s", path)
	}
	// Preserve meaningful transport-level detail rather than collapsing
	// every non-TLS failure to "cannot reach the server."
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Errorf("client: cannot resolve %q for %s: %s", dnsErr.Name, path, dnsErr.Err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("client: request timed out for %s", path)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("client: connection timed out for %s", path)
	}
	if detail := transportDetail(err); detail != "" {
		return fmt.Errorf("client: %s for %s", detail, path)
	}
	return fmt.Errorf("client: cannot reach the server for %s", path)
}

// transportDetail extracts a short reason from a *net.OpError without
// exposing the full URL or request metadata.
func transportDetail(err error) string {
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Err == nil {
		return ""
	}
	return opErr.Err.Error()
}

func certificateNames(certificate *x509.Certificate) []string {
	if certificate == nil {
		return []string{"an unreadable set of names"}
	}
	names := append([]string{}, certificate.DNSNames...)
	for _, address := range certificate.IPAddresses {
		names = append(names, address.String())
	}
	if len(names) == 0 && certificate.Subject.CommonName != "" {
		names = append(names, certificate.Subject.CommonName)
	}
	return names
}

func statusError(path string, response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
	detail := ""
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil {
		detail = strings.TrimSpace(payload.Error)
	}
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("client: the server rejected the token; check OBERTH_TOKEN or OBERTH_TOKEN_COMMAND (%s)", detail)
	case http.StatusForbidden:
		return fmt.Errorf("client: not permitted: %s", detail)
	case http.StatusNotFound:
		return fmt.Errorf("client: not found: %s (%s)", path, detail)
	}
	if response.StatusCode >= 500 {
		return fmt.Errorf("client: the server failed with %d on %s", response.StatusCode, path)
	}
	return fmt.Errorf("client: %s returned %d: %s", path, response.StatusCode, detail)
}
