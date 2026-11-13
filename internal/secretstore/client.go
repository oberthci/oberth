// Package secretstore fetches store-sourced pipeline secrets from an OpenBao
// (or Vault) KV backend. Only the Oberth server process ever holds secret store
// credentials: it authenticates with its own Kubernetes ServiceAccount token,
// reads exactly the administrator-allowlisted paths a repository declared, and
// hands the values to a step-scoped delivery channel. The runner never sees
// a secret store address, role, or token.
package secretstore

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/oberthci/oberth/pkg/pipeline"
)

const (
	// DefaultServiceAccountTokenPath is the in-cluster projected identity used
	// for the Kubernetes auth login.
	DefaultServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" // #nosec G101 -- well-known mount path, not a credential.
	// DefaultAuthMountPath is OpenBao's conventional Kubernetes auth mount.
	DefaultAuthMountPath = "kubernetes"
	// DefaultKVPath is the conventional KV v2 mount backing both the
	// administrator allowlist and the virtual oberth/upstream/ namespace.
	DefaultKVPath = "oberth"

	defaultTimeout          = 30 * time.Second
	maxTokenBytes           = 1 << 20
	maxFetchPaths           = 32
	loginPathVerbatim       = "auth/%s/login"
	kvDataField             = "data"
	kvResponseMetadataField = "metadata"
	maxSecretValueBytes     = 1 << 20
	maxSecretTotalBytes     = 8 << 20
	maxSecretKeysPerPath    = 256

	maxJSONOverhead       = 64 << 10
	maxSecretJSONResponse = 6*maxSecretTotalBytes + maxJSONOverhead
	maxAuthJSONResponse   = maxTokenBytes + maxJSONOverhead
	maxRevokeResponse     = 64 << 10
)

// Config carries the operator-supplied OpenBao connection settings.
type Config struct {
	// Address is the OpenBao API base URL. HTTPS is required; plain HTTP is
	// accepted only with the explicit AllowInsecureHTTP development override.
	Address string
	// AllowInsecureHTTP permits an http:// address for development setups
	// only. It never disables certificate verification for HTTPS.
	AllowInsecureHTTP bool
	// AuthMountPath is the Kubernetes auth mount, defaulting to "kubernetes".
	AuthMountPath string
	// Role is the OpenBao Kubernetes auth role bound to Oberth's
	// ServiceAccount.
	Role string
	// CACertPEM optionally pins the exact trust anchors for the secret store
	// connection. When set, the system pool is not consulted.
	CACertPEM []byte
	// ServiceAccountTokenPath overrides the projected token location.
	ServiceAccountTokenPath string
	// ServiceAccountTokenSource obtains a fresh projected identity in memory.
	// Ownership of every returned buffer (even on error) transfers to the
	// client, which clears it after login. Mutually exclusive with ServiceAccountTokenPath.
	ServiceAccountTokenSource func(context.Context) ([]byte, error)
	// Timeout bounds each HTTP round trip.
	Timeout time.Duration
}

// Client fetches KV secrets with a short-lived, per-fetch login.
type Client struct {
	api           *vaultapi.Client
	mount         string
	role          string
	tokenPath     string
	tokenSource   func(context.Context) ([]byte, error)
	verifiedHTTPS bool
}

// New validates the configuration and builds the deterministic API client.
// Environment variables never influence the connection: address, TLS trust,
// and authentication all come from explicit configuration.
func New(config Config) (*Client, error) {
	address := strings.TrimSpace(config.Address)
	if address == "" {
		return nil, errors.New("secret store address is required")
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("secret store address %q must be an absolute URL without credentials or a fragment", address)
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !config.AllowInsecureHTTP {
			return nil, fmt.Errorf("secret store address %q uses plain HTTP; use https:// or set the explicit insecure development override", address)
		}
	default:
		return nil, fmt.Errorf("secret store address %q must use https", address)
	}
	role := strings.TrimSpace(config.Role)
	if role == "" || strings.ContainsAny(role, "\x00\r\n/ ") {
		return nil, errors.New("secret store Kubernetes auth role is required and must be a single clean path segment")
	}
	mount := config.AuthMountPath
	if mount == "" {
		mount = DefaultAuthMountPath
	}
	if err := pipeline.ValidateSecretStorePath(mount); err != nil {
		return nil, fmt.Errorf("secret store Kubernetes auth mount: %w", err)
	}
	if config.ServiceAccountTokenSource != nil && config.ServiceAccountTokenPath != "" {
		return nil, errors.New("secret store ServiceAccount token source and path are mutually exclusive")
	}
	tokenPath := config.ServiceAccountTokenPath
	if tokenPath == "" {
		tokenPath = DefaultServiceAccountTokenPath
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13}
	if len(config.CACertPEM) != 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(config.CACertPEM) {
			return nil, errors.New("secret store CA bundle contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	transport := &http.Transport{
		TLSClientConfig:       tlsConfig,
		Proxy:                 nil, // never route secret store traffic through an environment-configured proxy
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       time.Minute,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	apiClient, err := vaultapi.NewClient(&vaultapi.Config{
		Address: address,
		HttpClient: &http.Client{
			Transport: boundedSecretStoreTransport{base: transport},
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		MaxRetries:       2,
		CheckRetry:       secretStoreRetryPolicy,
		DisableRedirects: true,
	})
	if err != nil {
		// SDK environment parsing can reflect supplied values or header names.
		return nil, errors.New("create secret store client failed")
	}
	resetSDKRequestIdentity(apiClient)
	return &Client{
		api: apiClient, mount: mount, role: role, tokenPath: tokenPath, tokenSource: config.ServiceAccountTokenSource,
		verifiedHTTPS: parsed.Scheme == "https",
	}, nil
}

// AI-CONTRACT: SDK construction and cloning import ambient request metadata.
// Reset each new instance before any request. Authentication comes only from
// our ServiceAccount login; namespaces, extra headers and response wrapping are
// not part of this client's contract. Do not mutate the process environment.
func resetSDKRequestIdentity(client *vaultapi.Client) {
	client.ClearToken()
	client.ClearNamespace()
	client.SetHeaders(http.Header{vaultapi.RequestHeaderName: {"true"}})
	client.SetWrappingLookupFunc(func(string, string) string { return "" })
}

// VerifyLogin performs a lightweight trust-chain probe: ServiceAccount token →
// Kubernetes-auth login → token revocation. It proves reachability, TLS/CA
// validation, unsealed state (a sealed store returns 503 on login), auth mount,
// role binding, and TokenReview — without reading any KV path or transit key.
// A timer-driven status probe must not cycle release credentials through server
// heap to light a dashboard LED.
func (client *Client) VerifyLogin(ctx context.Context) error {
	session, err := client.login(ctx)
	if err != nil {
		return err
	}
	client.logout(ctx, session)
	return nil
}

// SealStatus checks whether the secret store is sealed using the
// unauthenticated /v1/sys/seal-status endpoint. No authentication credentials
// are sent — the shared client has an explicit empty identity. The caller's
// context bounds the request timeout.
func (client *Client) SealStatus(ctx context.Context) (sealed bool, err error) {
	response, err := client.api.Sys().SealStatusWithContext(ctx)
	if err != nil {
		return false, sanitizeSecretStoreHTTPError("seal-status check", err)
	}
	return response.Sealed, nil
}

// FetchKV logs in with the Kubernetes ServiceAccount identity, reads every
// requested KV path, and returns path -> key -> value. Any unreachable secret store,
// failed login, missing path, non-string value, or oversized payload is an
// error: trusted admission fails closed and never uses a stale cache.
func (client *Client) FetchKV(ctx context.Context, paths []string) (map[string]map[string][]byte, error) {
	if len(paths) == 0 {
		return map[string]map[string][]byte{}, nil
	}
	if len(paths) > maxFetchPaths {
		return nil, fmt.Errorf("secret store fetch requests %d paths, maximum is %d", len(paths), maxFetchPaths)
	}
	for _, path := range paths {
		if err := pipeline.ValidateSecretStorePath(path); err != nil {
			return nil, err
		}
	}
	login, err := client.login(ctx)
	if err != nil {
		return nil, err
	}
	defer client.logout(ctx, login)

	return fetchKVPaths(paths, func(path string) (map[string][]byte, int, error) {
		return client.readKV(ctx, login, path)
	})
}

// fetchKVPaths transfers ownership only when every requested read succeeds.
// Failed or duplicate reads must not abandon secret buffers in the server heap.
func fetchKVPaths(paths []string, read func(string) (map[string][]byte, int, error)) (map[string]map[string][]byte, error) {
	result := make(map[string]map[string][]byte, len(paths))
	complete := false
	defer func() {
		if !complete {
			for _, values := range result {
				clearKVValues(values)
			}
		}
	}()
	total := 0
	for _, path := range paths {
		if _, seen := result[path]; seen {
			continue
		}
		values, size, err := read(path)
		result[path] = values
		if err != nil {
			return nil, err
		}
		total += size
		if total > maxSecretTotalBytes {
			return nil, fmt.Errorf("secret store secrets exceed %d bytes in total", maxSecretTotalBytes)
		}
	}
	complete = true
	return result, nil
}

func clearKVValues(values map[string][]byte) {
	for key, value := range values {
		clear(value)
		delete(values, key)
	}
}

func (client *Client) login(ctx context.Context) (*vaultapi.Client, error) {
	token, err := client.serviceAccountToken(ctx)
	if err != nil {
		return nil, err
	}
	defer clear(token) // Zero the SA token []byte after login completes.
	// A per-fetch clone keeps the login token off the shared client, so
	// concurrent fetches can never observe or race each other's identity.
	session, err := client.api.Clone()
	if err != nil {
		return nil, errors.New("prepare secret store session failed")
	}
	// Clone invokes the SDK constructor again and does not preserve the
	// wrapping callback. Reapply the entire request identity boundary.
	resetSDKRequestIdentity(session)
	response, err := session.Logical().WriteWithContext(ctx, fmt.Sprintf(loginPathVerbatim, client.mount), map[string]any{
		// Accepted residual: the immutable Go string produced by this conversion
		// cannot be zeroed (Go runtime/GC limitation). The source []byte (token)
		// is zeroed by the deferred clear above; the file-read buffer (raw) is
		// zeroed inside serviceAccountToken before it returns.
		"jwt":  string(token),
		"role": client.role,
	})
	if err != nil {
		return nil, sanitizeSecretStoreHTTPError("Kubernetes auth login", err)
	}
	if response == nil || response.Auth == nil || response.Auth.ClientToken == "" {
		return nil, errors.New("secret store Kubernetes auth login returned no client token")
	}
	session.SetToken(response.Auth.ClientToken)
	return session, nil
}

// logout revokes the short-lived login token. Revocation is best-effort: the
// values are already fetched and the token's own TTL bounds any residue, so a
// failed revoke must not fail the already-admitted phase.
func (client *Client) logout(ctx context.Context, session *vaultapi.Client) {
	if session == nil || session.Token() == "" {
		return
	}
	_ = session.Auth().Token().RevokeSelfWithContext(ctx, "")
	session.ClearToken()
}

func (client *Client) readKV(ctx context.Context, session *vaultapi.Client, path string) (map[string][]byte, int, error) {
	response, err := session.Logical().ReadWithContext(ctx, path)
	if err != nil {
		return nil, 0, fmt.Errorf("secret store entry %q is unavailable: %w", path, sanitizeSecretStoreHTTPError("KV read", err))
	}
	if response == nil || len(response.Data) == 0 {
		return nil, 0, fmt.Errorf("secret store entry %q is unavailable: not found", path)
	}
	data := response.Data
	// KV v2 read responses nest the entry under "data" beside "metadata".
	// A soft-deleted entry returns data.data as null (nil interface) or an
	// empty map; detect both shapes before falling through to the string
	// iteration that would produce a confusing "must be a string value" error.
	if _, hasMetadata := data[kvResponseMetadataField].(map[string]any); hasMetadata {
		rawData, hasDataKey := data[kvDataField]
		if hasDataKey {
			inner, isMap := rawData.(map[string]any)
			if !isMap || len(inner) == 0 {
				return nil, 0, fmt.Errorf("secret store entry %q is unavailable: secret path not found or deleted", path)
			}
			data = inner
		}
	}
	if len(data) > maxSecretKeysPerPath {
		return nil, 0, fmt.Errorf("secret store entry %q has %d keys, maximum is %d", path, len(data), maxSecretKeysPerPath)
	}
	// Validate every field before allocating mutable secret buffers.
	size := 0
	for key, raw := range data {
		text, ok := raw.(string)
		if !ok {
			return nil, 0, fmt.Errorf("secret store entry %q key %q must be a string value", path, key)
		}
		// An explicitly present empty string is a valid value, for example the
		// password of an unencrypted cosign key. Key presence and all byte bounds
		// remain enforced independently.
		if len(text) > maxSecretValueBytes {
			return nil, 0, fmt.Errorf("secret store entry %q key %q exceeds %d bytes", path, key, maxSecretValueBytes)
		}
		size += len(text)
	}
	values := make(map[string][]byte, len(data))
	for key, raw := range data {
		values[key] = []byte(raw.(string))
	}
	return values, size, nil
}

func (client *Client) serviceAccountToken(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if client.tokenSource != nil {
		raw, err := client.tokenSource(ctx)
		defer clear(raw)
		if err != nil {
			return nil, err
		}
		return copyServiceAccountToken(raw)
	}
	file, err := os.Open(client.tokenPath) // #nosec G304 -- the operator explicitly supplies this in-pod identity path.
	if err != nil {
		return nil, fmt.Errorf("read ServiceAccount token for secret store login: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("read ServiceAccount token for secret store login: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxTokenBytes {
		return nil, errors.New("ServiceAccount token for secret store login must be a non-empty bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxTokenBytes+1))
	defer clear(raw)
	if err != nil {
		return nil, fmt.Errorf("read ServiceAccount token for secret store login: %w", err)
	}
	return copyServiceAccountToken(raw)
}

func copyServiceAccountToken(raw []byte) ([]byte, error) {
	if len(raw) > maxTokenBytes {
		return nil, errors.New("ServiceAccount token for secret store login exceeds the size bound")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("ServiceAccount token for secret store login is empty")
	}
	return bytes.Clone(trimmed), nil
}
