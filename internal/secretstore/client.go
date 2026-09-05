// Package secretstore fetches store-sourced Release, Plan, and Apply secrets from an OpenBao
// (or Vault) KV backend. Only the Oberth server process ever holds secret store
// credentials: it authenticates with its own Kubernetes ServiceAccount token,
// reads exactly the administrator-allowlisted paths a repository declared, and
// hands the values to a phase-scoped Job delivery channel. The runner never sees
// a secret store address, role, or token.
package secretstore

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
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
	// DefaultKVMount is the conventional KV v2 mount backing both the
	// administrator allowlist and the virtual oberth/upstream/ namespace.
	DefaultKVMount = "oberth"
	// DefaultTransitMountPath is the dedicated engine used only for trusted
	// promotion artifact envelopes.
	DefaultTransitMountPath = "oberth-transit"
	// DefaultTransitKey is the non-exportable symmetric key used by that
	// engine. Repositories can never select either the mount or key.
	DefaultTransitKey = "trusted-plan-artifacts"

	defaultTimeout       = 30 * time.Second
	maxTokenBytes        = 1 << 20
	maxFetchPaths        = 32
	loginPathVerbatim    = "auth/%s/login"
	kvDataField          = "data"
	kvMetadataField      = "metadata"
	maxSecretValueBytes  = 1 << 20
	maxSecretTotalBytes  = 8 << 20
	maxSecretKeysPerPath = 256

	// A plan itself is capped at 16 MiB by the artifact store. Transit seals a
	// bounded manifest together with that plan under a 17 MiB envelope ceiling.
	// OpenBao ciphertext (and the durable SQLite column) has an independent
	// 32 MiB ceiling for base64 and Transit framing amplification.
	maxTransitPlaintextBytes  = 17 << 20 // plan + server-owned manifest
	maxTransitCiphertextBytes = 32 << 20 // durable OpenBao transit ciphertext ceiling
	maxTransitJSONOverhead    = 64 << 10
	maxSecretJSONResponse     = 6*maxSecretTotalBytes + maxTransitJSONOverhead
	maxAuthJSONResponse       = maxTokenBytes + maxTransitJSONOverhead
	maxRevokeResponse         = 64 << 10
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
	// TransitMountPath is the administrator-owned transit engine mount used
	// for trusted plan artifacts. It must be one clean path segment.
	TransitMountPath string
	// TransitKey is the administrator-owned transit key. Repositories never
	// control this value.
	TransitKey string
}

// Client fetches KV secrets with a short-lived, per-fetch login.
type Client struct {
	api           *vaultapi.Client
	mount         string
	role          string
	tokenPath     string
	transit       string
	transitKey    string
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
	if err := periapsis.ValidateSecretStorePath(mount); err != nil {
		return nil, fmt.Errorf("secret store Kubernetes auth mount: %w", err)
	}
	transit := config.TransitMountPath
	if transit == "" {
		transit = DefaultTransitMountPath
	}
	if err := validateTransitSegment("mount", transit); err != nil {
		return nil, err
	}
	transitKey := config.TransitKey
	if transitKey == "" {
		transitKey = DefaultTransitKey
	}
	if err := validateTransitSegment("key", transitKey); err != nil {
		return nil, err
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
		return nil, fmt.Errorf("create secret store client: %w", err)
	}
	// NewClient adopts VAULT_TOKEN and VAULT_NAMESPACE from the environment
	// when present; this client authenticates per fetch with the
	// ServiceAccount identity only and uses no namespace header.
	apiClient.ClearToken()
	apiClient.ClearNamespace()
	return &Client{
		api: apiClient, mount: mount, role: role, tokenPath: tokenPath,
		transit: transit, transitKey: transitKey, verifiedHTTPS: parsed.Scheme == "https",
	}, nil
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
// are sent — the shared client has ClearToken since construction. The caller's
// context bounds the request timeout.
func (client *Client) SealStatus(ctx context.Context) (sealed bool, err error) {
	response, err := client.api.Sys().SealStatusWithContext(ctx)
	if err != nil {
		return false, sanitizeSecretStoreHTTPError("seal-status check", err)
	}
	return response.Sealed, nil
}

// Encrypt seals one non-empty trusted-plan envelope with the fixed,
// administrator-owned OpenBao transit key. Even when KV development access
// explicitly permits HTTP, trusted plan bytes never cross an unverified
// transport.
func (client *Client) Encrypt(ctx context.Context, plaintext []byte) (string, error) {
	if err := client.requireVerifiedHTTPS("transit encrypt"); err != nil {
		return "", err
	}
	if err := validateTransitPlaintextSize(len(plaintext)); err != nil {
		return "", err
	}
	session, err := client.login(ctx)
	if err != nil {
		return "", err
	}
	defer client.logout(ctx, session)

	response, err := client.transitWrite(ctx, session, "encrypt", map[string][]byte{
		"plaintext": plaintext,
	}, int64(maxTransitCiphertextBytes+maxTransitJSONOverhead))
	if err != nil {
		return "", err
	}
	defer clear(response)
	var envelope struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return "", fmt.Errorf("secret store transit encrypt returned malformed JSON: %w", err)
	}
	if envelope.Data.Ciphertext == "" {
		return "", errors.New("secret store transit encrypt returned no ciphertext")
	}
	if err := validateTransitCiphertextSize(len(envelope.Data.Ciphertext)); err != nil {
		return "", err
	}
	return envelope.Data.Ciphertext, nil
}

// Decrypt opens one bounded trusted-plan envelope. The caller must still
// validate the embedded manifest before making the plaintext available to an
// Apply runner.
func (client *Client) Decrypt(ctx context.Context, ciphertext string) ([]byte, error) {
	if err := client.requireVerifiedHTTPS("transit decrypt"); err != nil {
		return nil, err
	}
	if err := validateTransitCiphertextSize(len(ciphertext)); err != nil {
		return nil, err
	}
	session, err := client.login(ctx)
	if err != nil {
		return nil, err
	}
	defer client.logout(ctx, session)

	encodedLimit := int64(base64.StdEncoding.EncodedLen(maxTransitPlaintextBytes) + maxTransitJSONOverhead)
	response, err := client.transitWrite(ctx, session, "decrypt", map[string]string{
		"ciphertext": ciphertext,
	}, encodedLimit)
	if err != nil {
		return nil, err
	}
	defer clear(response)
	var envelope struct {
		Data struct {
			Plaintext []byte `json:"plaintext"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return nil, fmt.Errorf("secret store transit decrypt returned malformed JSON: %w", err)
	}
	if len(envelope.Data.Plaintext) == 0 {
		return nil, errors.New("secret store transit decrypt returned no plaintext")
	}
	if err := validateTransitPlaintextSize(len(envelope.Data.Plaintext)); err != nil {
		return nil, err
	}
	return envelope.Data.Plaintext, nil
}

func (client *Client) transitWrite(
	ctx context.Context,
	session *vaultapi.Client,
	operation string,
	body any,
	responseLimit int64,
) ([]byte, error) {
	encodedBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	defer clear(encodedBody)
	response, err := session.Logical().WriteRawWithContext(
		ctx,
		fmt.Sprintf("%s/%s/%s", client.transit, operation, client.transitKey),
		encodedBody,
	)
	if err != nil {
		return nil, sanitizeSecretStoreHTTPError("transit "+operation, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil {
		return nil, errors.New("secret store transit " + operation + " failed: response read error")
	}
	if int64(len(raw)) > responseLimit {
		clear(raw)
		return nil, fmt.Errorf("secret store transit %s failed: response exceeds %d bytes", operation, responseLimit)
	}
	return raw, nil
}

func validateTransitSegment(label, value string) error {
	if strings.Contains(value, "/") {
		return fmt.Errorf("secret store transit %s is required and must be a single clean path segment", label)
	}
	if err := periapsis.ValidateSecretStorePath(value); err != nil {
		return fmt.Errorf("secret store transit %s: %w", label, err)
	}
	return nil
}

func validateTransitPlaintextSize(size int) error {
	if size <= 0 {
		return errors.New("secret store transit plaintext must be non-empty")
	}
	if size > maxTransitPlaintextBytes {
		return fmt.Errorf("secret store transit plaintext exceeds %d bytes", maxTransitPlaintextBytes)
	}
	return nil
}

func validateTransitCiphertextSize(size int) error {
	if size <= 0 {
		return errors.New("secret store transit ciphertext must be non-empty")
	}
	if size > maxTransitCiphertextBytes {
		return fmt.Errorf("secret store transit ciphertext exceeds %d bytes", maxTransitCiphertextBytes)
	}
	return nil
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
	defer clear(token) // Zero the SA token []byte after login completes.
	// A per-fetch clone keeps the login token off the shared client, so
	// concurrent fetches can never observe or race each other's identity.
	session, err := client.api.Clone()
	if err != nil {
		return nil, fmt.Errorf("prepare secret store session: %w", err)
	}
	session.ClearToken()
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
	if _, hasMetadata := data[kvMetadataField].(map[string]any); hasMetadata {
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
	values := make(map[string][]byte, len(data))
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
		values[key] = []byte(text)
		size += len(text)
	}
	return values, size, nil
}

func (client *Client) serviceAccountToken() ([]byte, error) {
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
	if err != nil {
		return nil, fmt.Errorf("read ServiceAccount token for secret store login: %w", err)
	}
	if len(raw) > maxTokenBytes {
		clear(raw)
		return nil, errors.New("ServiceAccount token for secret store login exceeds the size bound")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		clear(raw)
		return nil, errors.New("ServiceAccount token for secret store login is empty")
	}
	token := bytes.Clone(trimmed)
	clear(raw) // Zero the file-read buffer; trimmed shares its backing array.
	return token, nil
}
