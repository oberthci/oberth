// Package secretstore configures static account-scoped store access and delivers bounded secrets.
//
// Spec: docs/plans/c9-install-and-upgrade.md#invariants
package secretstore

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/runtimeconfig"
)

const MaxValueBytes = model.MaxSecretValueBytes
const maxBody = 2 << 20

type Connection struct {
	URL        string
	ServerName string
	CA         []byte
	Transport  http.RoundTripper
}
type Client struct {
	http    *http.Client
	address string
}
type Request struct {
	Query  url.Values
	Method string
	Path   string
	Token  []byte
	Body   any
	Accept int
}

func New(c Connection) (*Client, error) {
	if !runtimeconfig.ValidStoreAddress(c.URL) {
		return nil, refuse("address", nil)
	}
	transport := c.Transport
	if transport == nil {
		roots := x509.NewCertPool()
		if c.ServerName == "" || !roots.AppendCertsFromPEM(c.CA) {
			return nil, refuse("trust", nil)
		}
		transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: c.ServerName}}
	}
	return &Client{address: strings.TrimSuffix(c.URL, "/"), http: &http.Client{
		Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// JSON never returns a response body as a diagnostic: it may contain secrets.
func (c *Client) JSON(ctx context.Context, r Request, out any) (int, error) {
	if !validRequestPath(r.Path) {
		return 0, refuse("path", nil)
	}
	body, err := json.Marshal(r.Body)
	if err != nil {
		return 0, refuse("input", err)
	}
	defer clear(body)
	if len(body) > maxBody {
		return 0, refuse("limit", nil)
	}
	address := c.address + "/v1" + r.Path
	if len(r.Query) > 0 {
		address += "?" + r.Query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, address, bytes.NewReader(body))
	if err != nil {
		return 0, refuse("address", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if len(r.Token) > 0 {
		req.Header.Set("X-Vault-Token", string(r.Token))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, refuse("connect", ctxCause(ctx, err))
	}
	defer func() { _ = resp.Body.Close() }()
	if r.Path == brokerPath && resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get("Retry-After") == "1" {
		return resp.StatusCode, errBrokerCapacity
	}
	return decodeResponse(resp, out, r.Accept, r.Path)
}

func decodeResponse(resp *http.Response, out any, accepted int, path string) (int, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	defer clear(raw)
	if err != nil {
		return resp.StatusCode, refuse("connect", err)
	}
	if len(raw) > maxBody {
		return resp.StatusCode, refuse("limit", err)
	}
	if resp.StatusCode == accepted {
		return resp.StatusCode, nil
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		return resp.StatusCode, refuse("sealed", nil)
	}
	if resp.StatusCode >= 500 {
		return resp.StatusCode, refuse("connect", nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		problem := refuse("request", fmt.Errorf("HTTP %d", resp.StatusCode))
		if backupShareRejected(path, resp.StatusCode, raw) {
			problem.ID = "backup-share-rejected"
		}
		return resp.StatusCode, problem
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return resp.StatusCode, refuse("response", nil)
	}
	return resp.StatusCode, nil
}

func ctxCause(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
func (c *Client) Close() { c.http.CloseIdleConnections() }
func refuse(code string, cause error) *publicerr.Error {
	reason, remedy := refusal(code)
	if code == "configuration" {
		reason, remedy = runtimeconfig.SecretStoreConfiguration()
	}
	kind := publicerr.InvalidInput
	if code == "sealed" || code == "configuration" || code == "connect" || code == "broker" || code == "response" {
		kind = publicerr.Unavailable
	}
	return publicerr.New(kind, reason, remedy, cause)
}

func validRequestPath(path string) bool {
	if strings.ContainsAny(path, "?#\\\x00") || strings.Contains(path, "..") || !strings.HasPrefix(path, "/") {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." {
			return false
		}
	}
	return true
}
