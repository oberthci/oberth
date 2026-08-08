// Package secretstore fetches store-sourced release secrets from an OpenBao
// (or Vault) KV backend. Only the Oberth server process ever holds secret store
// credentials: it authenticates with its own Kubernetes ServiceAccount token,
// reads exactly the administrator-allowlisted paths a repository declared, and
// hands the values to the release-Job delivery channel. The runner never sees
// a secret store address, role, or token.
package secretstore

import (
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

	"github.com/oberthci/oberth/pkg/periapsis"
)

const (
	// DefaultServiceAccountTokenPath is the in-cluster projected identity used
	// for the Kubernetes auth login.
	DefaultServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" // #nosec G101 -- well-known mount path, not a credential.
	// DefaultAuthMountPath is OpenBao's conventional Kubernetes auth mount.
	DefaultAuthMountPath = "kubernetes"

	defaultTimeout       = 30 * time.Second
	maxTokenBytes        = 1 << 20
	maxFetchPaths        = 32
	loginPathVerbatim    = "auth/%s/login"
	kvDataField          = "data"
	kvMetadataField      = "metadata"
	maxSecretValueBytes  = 1 << 20
	maxSecretTotalBytes  = 8 << 20
	maxSecretKeysPerPath = 256
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
	// Timeout bounds each HTTP round trip.
	Timeout time.Duration
}

// Client fetches KV secrets with a short-lived, per-fetch login.
type Client struct {
	api       *vaultapi.Client
	mount     string
	role      string
	tokenPath string
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
	if err := periapsis.ValidateSecretStorePath(mount); err != nil {
		return nil, fmt.Errorf("secret store Kubernetes auth mount: %w", err)
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
		Address:    address,
		HttpClient: &http.Client{Transport: transport, Timeout: timeout},
		MaxRetries: 2,
	})
	if err != nil {
		return nil, fmt.Errorf("create secret store client: %w", err)
	}
	// NewClient adopts VAULT_TOKEN and VAULT_NAMESPACE from the environment
	// when present; this client authenticates per fetch with the
	// ServiceAccount identity only and uses no namespace header.
	apiClient.ClearToken()
	apiClient.ClearNamespace()
	return &Client{api: apiClient, mount: mount, role: role, tokenPath: tokenPath}, nil
}

// FetchKV logs in with the Kubernetes ServiceAccount identity, reads every
// requested KV path, and returns path -> key -> value. Any unreachable secret store,
// failed login, missing path, non-string value, or oversized payload is an
// error: release admission fails closed and never uses a stale cache.
func (client *Client) FetchKV(ctx context.Context, paths []string) (map[string]map[string][]byte, error) {
	if len(paths) == 0 {
		return map[string]map[string][]byte{}, nil
	}
	if len(paths) > maxFetchPaths {
		return nil, fmt.Errorf("secret store fetch requests %d paths, maximum is %d", len(paths), maxFetchPaths)
	}
	for _, path := range paths {
		if err := periapsis.ValidateSecretStorePath(path); err != nil {
			return nil, err
		}
	}
	login, err := client.login(ctx)
	if err != nil {
		return nil, err
	}
	defer client.logout(ctx, login)

	result := make(map[string]map[string][]byte, len(paths))
	total := 0
	for _, path := range paths {
		values, size, err := client.readKV(ctx, login, path)
		if err != nil {
			return nil, err
		}
		total += size
		if total > maxSecretTotalBytes {
			return nil, fmt.Errorf("secret store secrets exceed %d bytes in total", maxSecretTotalBytes)
		}
		result[path] = values
	}
	return result, nil
}

func (client *Client) login(ctx context.Context) (*vaultapi.Client, error) {
	token, err := client.serviceAccountToken()
	if err != nil {
		return nil, err
	}
	// A per-fetch clone keeps the login token off the shared client, so
	// concurrent fetches can never observe or race each other's identity.
	session, err := client.api.Clone()
	if err != nil {
		return nil, fmt.Errorf("prepare secret store session: %w", err)
	}
	session.ClearToken()
	response, err := session.Logical().WriteWithContext(ctx, fmt.Sprintf(loginPathVerbatim, client.mount), map[string]any{
		"jwt":  token,
		"role": client.role,
	})
	if err != nil {
		return nil, fmt.Errorf("secret store Kubernetes auth login at mount %q for role %q failed: %w", client.mount, client.role, err)
	}
	if response == nil || response.Auth == nil || response.Auth.ClientToken == "" {
		return nil, errors.New("secret store Kubernetes auth login returned no client token")
	}
	session.SetToken(response.Auth.ClientToken)
	return session, nil
}

// logout revokes the short-lived login token. Revocation is best-effort: the
// values are already fetched and the token's own TTL bounds any residue, so a
// failed revoke must not fail the release.
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
		return nil, 0, fmt.Errorf("secret store entry %q is unavailable: %w", path, err)
	}
	if response == nil || len(response.Data) == 0 {
		return nil, 0, fmt.Errorf("secret store entry %q is unavailable: not found", path)
	}
	data := response.Data
	// KV v2 read responses nest the entry under "data" beside "metadata".
	if inner, ok := data[kvDataField].(map[string]any); ok {
		if _, hasMetadata := data[kvMetadataField].(map[string]any); hasMetadata {
			if len(inner) == 0 {
				return nil, 0, fmt.Errorf("secret store entry %q is unavailable: the entry is deleted or has no data", path)
			}
			data = inner
		}
	}
	if len(data) > maxSecretKeysPerPath {
		return nil, 0, fmt.Errorf("secret store entry %q has %d keys, maximum is %d", path, len(data), maxSecretKeysPerPath)
	}
	values := make(map[string][]byte, len(data))
	size := 0
	for key, raw := range data {
		text, ok := raw.(string)
		if !ok {
			return nil, 0, fmt.Errorf("secret store entry %q key %q must be a string value", path, key)
		}
		if text == "" {
			return nil, 0, fmt.Errorf("secret store entry %q key %q has an empty value", path, key)
		}
		if len(text) > maxSecretValueBytes {
			return nil, 0, fmt.Errorf("secret store entry %q key %q exceeds %d bytes", path, key, maxSecretValueBytes)
		}
		values[key] = []byte(text)
		size += len(text)
	}
	return values, size, nil
}

func (client *Client) serviceAccountToken() (string, error) {
	file, err := os.Open(client.tokenPath) // #nosec G304 -- the operator explicitly supplies this in-pod identity path.
	if err != nil {
		return "", fmt.Errorf("read ServiceAccount token for secret store login: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("read ServiceAccount token for secret store login: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxTokenBytes {
		return "", errors.New("ServiceAccount token for secret store login must be a non-empty bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxTokenBytes+1))
	if err != nil {
		return "", fmt.Errorf("read ServiceAccount token for secret store login: %w", err)
	}
	if len(raw) > maxTokenBytes {
		return "", errors.New("ServiceAccount token for secret store login exceeds the size bound")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("ServiceAccount token for secret store login is empty")
	}
	return token, nil
}
