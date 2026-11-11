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

	"github.com/oberthci/oberth/internal/mdnsdial"
	"github.com/oberthci/oberth/internal/recordjson"
	"github.com/oberthci/oberth/internal/restartretry"
)

const (
	requestTimeout  = 30 * time.Second
	maxResponseSize = 8 << 20
	maxErrorBody    = 4 << 10
)

type Config struct {
	ClientCert   string
	ClientKey    string
	BaseURL      string
	Token        string
	TokenCommand string
	CACert       string
}

func FromEnv() Config {
	return Config{
		ClientCert:   strings.TrimSpace(os.Getenv("OBERTH_CLIENT_CERT")),
		ClientKey:    strings.TrimSpace(os.Getenv("OBERTH_CLIENT_KEY")),
		BaseURL:      strings.TrimSpace(os.Getenv("OBERTH_BASE_URL")),
		Token:        strings.TrimSpace(os.Getenv("OBERTH_TOKEN")),
		TokenCommand: strings.TrimSpace(os.Getenv("OBERTH_TOKEN_COMMAND")),
		CACert:       strings.TrimSpace(os.Getenv("OBERTH_CA_CERT")),
	}
}

// ResolveToken shares credential precedence between REST and MCP clients.
// The caller's context bounds execution of an operator-configured command.
func (config Config) ResolveToken(ctx context.Context) (string, error) {
	if config.Token != "" {
		return config.Token, nil
	}
	if config.TokenCommand == "" && config.ClientCert != "" && config.ClientKey != "" {
		return "", nil
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
	base, err := ValidateServerURL(config.BaseURL)
	if err != nil {
		return nil, err
	}
	if config.ClientCert != "" && base.Scheme != "https" {
		return nil, errors.New("client certificate authentication requires https")
	}
	token, err := config.ResolveToken(ctx)
	if err != nil {
		return nil, err
	}
	transport, err := newTransport(config.CACert)
	if err != nil {
		return nil, err
	}
	transport, err = WithClientCertificate(transport, config.ClientCert, config.ClientKey)
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

// ValidateServerURL applies the shared REST and MCP transport policy before
// resolving credentials or constructing a transport.
func ValidateServerURL(address string) (*url.URL, error) {
	if address == "" {
		return nil, errors.New("client: set OBERTH_BASE_URL to the server's https address")
	}
	base, err := url.Parse(address)
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
	return base, nil
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
	// A .local server name resolves through the system resolver when Go's
	// own cannot (#066).
	cloned.DialContext = mdnsdial.Dialer{Base: &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}}.DialContext
	if cloned.TLSClientConfig == nil {
		cloned.TLSClientConfig = &tls.Config{}
	}
	cloned.TLSClientConfig.MinVersion = tls.VersionTLS13
	if caCert == "" {
		return cloned, nil
	}
	pool, err := LoadCAPool(caCert)
	if err != nil {
		return nil, fmt.Errorf("client: OBERTH_CA_CERT: %w", err)
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
	if client.token != "" {
		request.Header.Set("Authorization", "Bearer "+client.token)
	}
	return request, nil
}

func (client *Client) Get(ctx context.Context, path string, query map[string]string, into any) error {
	_, err := client.GetWithHeaders(ctx, path, query, into)
	return err
}

// GetWithHeaders returns the headers from the successful response, including
// the server's authoritative paging cursor, with the same retry/read bounds.
func (client *Client) GetWithHeaders(ctx context.Context, path string, query map[string]string, into any) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var budget restartretry.Budget
	for {
		var headers http.Header
		err, retry := client.getOnceHeaders(ctx, path, query, into, &headers)
		if err == nil || !retry {
			return headers, err
		}
		if !budget.Wait(ctx) {
			return nil, errors.Join(err, ctx.Err())
		}
	}
}

func (client *Client) getOnceHeaders(ctx context.Context, path string, query map[string]string, into any, headers *http.Header) (error, bool) {
	request, err := client.newGetRequest(ctx, path, query)
	if err != nil {
		return err, false
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return classifyTransport(path, err), restartretry.Transient(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return statusError(path, response), restartretry.Status(response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize+1))
	if err != nil {
		return fmt.Errorf("client: read %s: %w", path, err), restartretry.Transient(err)
	}
	if len(body) > maxResponseSize {
		return fmt.Errorf("client: %s response too large", path), false
	}
	if headers != nil {
		*headers = response.Header.Clone()
	}
	if into == nil {
		return nil, false
	}
	if err := recordjson.Unmarshal(body, into); err != nil {
		return fmt.Errorf("client: %s returned a body that is not the expected JSON", path), false
	}
	return nil, false
}

func (client *Client) GetRaw(ctx context.Context, path string, query map[string]string) ([]byte, error) {
	var raw json.RawMessage
	if err := client.Get(ctx, path, query, &raw); err != nil {
		return nil, err
	}
	return raw, nil
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
		if mdnsdial.IsLocalName(dnsErr.Name) {
			return fmt.Errorf("client: cannot resolve %q for %s (%s)", dnsErr.Name, path, mdnsdial.Hint)
		}
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
