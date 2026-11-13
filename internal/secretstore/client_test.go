package secretstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type mockVault struct {
	*httptest.Server
	caPEM      []byte
	tokenPath  string
	logins     atomic.Int64
	revokes    atomic.Int64
	badLogins  atomic.Int64
	reads      atomic.Int64
	sealed     atomic.Bool
	sealChecks atomic.Int64
}

const testLoginToken = "s.test-login-token"
const testServiceJWT = "header.payload.signature"

func newMockVault(t *testing.T) *mockVault {
	t.Helper()
	mock := &mockVault{}
	handler := http.NewServeMux()
	handler.HandleFunc("PUT /v1/auth/kubernetes/login", func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["jwt"] != testServiceJWT {
			mock.badLogins.Add(1)
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(`{"errors":["invalid role or jwt"]}`))
			return
		}
		if body["role"] == "error-reflection" {
			writer.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"errors": []string{strings.Repeat("oversized-login-error-", 1<<17) + testServiceJWT},
			})
			return
		}
		if body["role"] != "oberth" {
			mock.badLogins.Add(1)
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(`{"errors":["invalid role or jwt"]}`))
			return
		}
		mock.logins.Add(1)
		_, _ = writer.Write([]byte(`{"auth":{"client_token":"` + testLoginToken + `","lease_duration":60}}`))
	})
	authenticated := func(writer http.ResponseWriter, request *http.Request) bool {
		if request.Header.Get("X-Vault-Token") != testLoginToken {
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(`{"errors":["permission denied"]}`))
			return false
		}
		return true
	}
	handler.HandleFunc("GET /v1/ci/data/r2-upload", func(writer http.ResponseWriter, request *http.Request) {
		if !authenticated(writer, request) {
			return
		}
		mock.reads.Add(1)
		_, _ = writer.Write([]byte(`{"data":{"data":{"access":"r2-access","secret":"r2-secret"},"metadata":{"version":3}}}`))
	})
	handler.HandleFunc("GET /v1/ci/data/cosign-secret", func(writer http.ResponseWriter, request *http.Request) {
		if !authenticated(writer, request) {
			return
		}
		mock.reads.Add(1)
		_, _ = writer.Write([]byte(`{"data":{"data":{"cosign.key":"private-key","cosign.password":""},"metadata":{"version":1}}}`))
	})
	handler.HandleFunc("GET /v1/legacy/gar", func(writer http.ResponseWriter, request *http.Request) {
		if !authenticated(writer, request) {
			return
		}
		mock.reads.Add(1)
		_, _ = writer.Write([]byte(`{"data":{"token":"gar-token-value"}}`))
	})
	handler.HandleFunc("GET /v1/ci/data/broken", func(writer http.ResponseWriter, request *http.Request) {
		if !authenticated(writer, request) {
			return
		}
		_, _ = writer.Write([]byte(`{"data":{"data":{"count":5},"metadata":{"version":1}}}`))
	})
	handler.HandleFunc("GET /v1/ci/data/deleted", func(writer http.ResponseWriter, request *http.Request) {
		if !authenticated(writer, request) {
			return
		}
		_, _ = writer.Write([]byte(`{"data":{"data":{},"metadata":{"deletion_time":"2026-08-08T00:00:00Z"}}}`))
	})
	handler.HandleFunc("GET /v1/ci/data/soft-deleted-null", func(writer http.ResponseWriter, request *http.Request) {
		if !authenticated(writer, request) {
			return
		}
		_, _ = writer.Write([]byte(`{"data":{"data":null,"metadata":{"deletion_time":"2026-08-08T00:00:00Z","version":3}}}`))
	})
	handler.HandleFunc("PUT /v1/auth/token/revoke-self", func(writer http.ResponseWriter, request *http.Request) {
		if !authenticated(writer, request) {
			return
		}
		mock.revokes.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	})
	handler.HandleFunc("GET /v1/sys/seal-status", func(writer http.ResponseWriter, request *http.Request) {
		mock.sealChecks.Add(1)
		sealed := mock.sealed.Load()
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"type": "shamir", "initialized": true, "sealed": sealed,
			"t": 1, "n": 1, "progress": 0, "nonce": "",
		})
	})
	handler.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"errors":[]}`))
	})
	mock.Server = httptest.NewTLSServer(handler)
	t.Cleanup(mock.Close)
	mock.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: mock.Server.Certificate().Raw})
	mock.tokenPath = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(mock.tokenPath, []byte(testServiceJWT+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return mock
}

func (mock *mockVault) config() Config {
	return Config{
		Address:                 mock.URL,
		Role:                    "oberth",
		CACertPEM:               mock.caPEM,
		ServiceAccountTokenPath: mock.tokenPath,
	}
}

func TestSecretStoreNeverFollowsCrossOriginRedirects(t *testing.T) {
	for _, redirectAt := range []string{"login", "kv"} {
		t.Run(redirectAt, func(t *testing.T) {
			const plaintext = "kv-plaintext-must-not-cross-origin"
			var targetRequests atomic.Int64
			var targetObservedSecret atomic.Bool
			target := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				targetRequests.Add(1)
				body, _ := io.ReadAll(request.Body)
				if strings.Contains(string(body), plaintext) ||
					strings.Contains(string(body), testServiceJWT) ||
					request.Header.Get("X-Vault-Token") == testLoginToken {
					targetObservedSecret.Store(true)
				}
				writer.WriteHeader(http.StatusNoContent)
			}))
			defer target.Close()

			sourceMux := http.NewServeMux()
			sourceMux.HandleFunc("PUT /v1/auth/kubernetes/login", func(writer http.ResponseWriter, request *http.Request) {
				if redirectAt == "login" {
					http.Redirect(writer, request, target.URL+"/login-leak", http.StatusTemporaryRedirect)
					return
				}
				_, _ = writer.Write([]byte(`{"auth":{"client_token":"` + testLoginToken + `","lease_duration":60}}`))
			})
			sourceMux.HandleFunc("GET /v1/oberth/data/release/test", func(writer http.ResponseWriter, request *http.Request) {
				http.Redirect(writer, request, target.URL+"/kv-leak", http.StatusTemporaryRedirect)
			})
			sourceMux.HandleFunc("PUT /v1/auth/token/revoke-self", func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(http.StatusNoContent)
			})
			source := httptest.NewTLSServer(sourceMux)
			defer source.Close()

			caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: source.Certificate().Raw})
			caPEM = append(caPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: target.Certificate().Raw})...)
			tokenPath := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenPath, []byte(testServiceJWT), 0o600); err != nil {
				t.Fatal(err)
			}
			client, err := New(Config{
				Address: source.URL, Role: "oberth", CACertPEM: caPEM,
				ServiceAccountTokenPath: tokenPath,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.FetchKV(context.Background(), []string{"oberth/data/release/test"})
			if err == nil || !strings.Contains(err.Error(), "HTTP 307 Temporary Redirect") {
				t.Fatalf("FetchKV() error = %v, want sanitized 307", err)
			}
			for _, forbidden := range []string{plaintext, testServiceJWT, testLoginToken, target.URL} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("FetchKV() error disclosed %q: %v", forbidden, err)
				}
			}
			if got := targetRequests.Load(); got != 0 {
				t.Fatalf("redirect target requests = %d, want 0", got)
			}
			if targetObservedSecret.Load() {
				t.Fatal("redirect target observed login or KV secret material")
			}
		})
	}
}

func TestLoginNonSuccessResponseIsBoundedAndNeverReflected(t *testing.T) {
	t.Parallel()
	mock := newMockVault(t)
	config := mock.config()
	config.Role = "error-reflection"
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	_, loginErr := client.FetchKV(context.Background(), []string{"oberth/data/release/test"})
	assertSanitizedHTTPError(t, loginErr, http.StatusBadGateway, testServiceJWT, "oversized-login-error")
	if mock.reads.Load() != 0 {
		t.Fatal("Transit request ran after failed Kubernetes auth")
	}
}

func TestFetchKVReadsBothKVShapesWithShortLivedLogin(t *testing.T) {
	t.Parallel()
	mock := newMockVault(t)
	client, err := New(mock.config())
	if err != nil {
		t.Fatal(err)
	}
	values, err := client.FetchKV(context.Background(), []string{"ci/data/r2-upload", "legacy/gar"})
	if err != nil {
		t.Fatal(err)
	}
	if string(values["ci/data/r2-upload"]["access"]) != "r2-access" || string(values["ci/data/r2-upload"]["secret"]) != "r2-secret" {
		t.Fatalf("kv v2 values = %q", values["ci/data/r2-upload"])
	}
	if string(values["legacy/gar"]["token"]) != "gar-token-value" {
		t.Fatalf("kv v1 values = %q", values["legacy/gar"])
	}
	if mock.logins.Load() != 1 || mock.reads.Load() != 2 {
		t.Fatalf("logins = %d, reads = %d", mock.logins.Load(), mock.reads.Load())
	}
	if mock.revokes.Load() != 1 {
		t.Fatalf("login token was not revoked after the fetch: revokes = %d", mock.revokes.Load())
	}
}

func TestFetchKVPreservesPresentEmptyValues(t *testing.T) {
	t.Parallel()
	mock := newMockVault(t)
	client, err := New(mock.config())
	if err != nil {
		t.Fatal(err)
	}
	values, err := client.FetchKV(context.Background(), []string{"ci/data/cosign-secret"})
	if err != nil {
		t.Fatal(err)
	}
	password, present := values["ci/data/cosign-secret"]["cosign.password"]
	if !present || len(password) != 0 {
		t.Fatalf("empty password = %q, present=%v", password, present)
	}
	if string(values["ci/data/cosign-secret"]["cosign.key"]) != "private-key" {
		t.Fatalf("cosign key = %q", values["ci/data/cosign-secret"]["cosign.key"])
	}
}

func TestFetchKVFailsClosed(t *testing.T) {
	t.Parallel()
	mock := newMockVault(t)
	client, err := New(mock.config())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "missing entry", path: "ci/data/does-not-exist", want: `secret store entry "ci/data/does-not-exist" is unavailable: not found`},
		{name: "non-string value", path: "ci/data/broken", want: "must be a string value"},
		{name: "deleted kv v2 entry", path: "ci/data/deleted", want: "secret path not found or deleted"},
		{name: "soft-deleted null data", path: "ci/data/soft-deleted-null", want: "secret path not found or deleted"},
		{name: "reserved path", path: "sys/raw/ci", want: "reserved"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := client.FetchKV(context.Background(), []string{test.path})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestFetchKVReportsLoginFailure(t *testing.T) {
	t.Parallel()
	mock := newMockVault(t)
	config := mock.config()
	config.Role = "wrong-role"
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.FetchKV(context.Background(), []string{"ci/data/r2-upload"})
	if err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("login failure error = %v", err)
	}
	if mock.badLogins.Load() != 1 || mock.reads.Load() != 0 {
		t.Fatalf("failed login must not read secrets: badLogins=%d reads=%d", mock.badLogins.Load(), mock.reads.Load())
	}
}

func TestFetchKVFailsWhenVaultIsUnreachable(t *testing.T) {
	t.Parallel()
	mock := newMockVault(t)
	config := mock.config()
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	mock.Close()
	_, err = client.FetchKV(context.Background(), []string{"ci/data/r2-upload"})
	if err == nil {
		t.Fatal("unreachable secret store must fail the fetch")
	}
}

func TestNewEnforcesExplicitSecureConfiguration(t *testing.T) {
	t.Parallel()
	base := Config{Address: "https://openbao.openbao.svc:8200", Role: "oberth"}
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "missing address", mutate: func(c *Config) { c.Address = "" }, want: "address is required"},
		{name: "plain http", mutate: func(c *Config) { c.Address = "http://openbao.openbao.svc:8200" }, want: "plain HTTP"},
		{name: "other scheme", mutate: func(c *Config) { c.Address = "ftp://openbao:8200" }, want: "must use https"},
		{name: "hostless address", mutate: func(c *Config) { c.Address = "unix:///tmp/vault.sock" }, want: "absolute URL"},
		{name: "credentials in URL", mutate: func(c *Config) { c.Address = "https://user:pass@openbao:8200" }, want: "without credentials"},
		{name: "missing role", mutate: func(c *Config) { c.Role = "" }, want: "role is required"},
		{name: "role with slash", mutate: func(c *Config) { c.Role = "a/b" }, want: "single clean path segment"},
		{name: "reserved auth mount grammar", mutate: func(c *Config) { c.AuthMountPath = "bad mount" }, want: "only ASCII"},
		{name: "empty CA bundle", mutate: func(c *Config) { c.CACertPEM = []byte("not a certificate") }, want: "no certificates"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := base
			test.mutate(&config)
			if _, err := New(config); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestNewAllowsHTTPOnlyWithExplicitDevelopmentOverride(t *testing.T) {
	t.Parallel()
	config := Config{Address: "http://127.0.0.1:8200", Role: "oberth", AllowInsecureHTTP: true}
	_, err := New(config)
	if err != nil {
		t.Fatalf("explicit insecure development override rejected: %v", err)
	}

}

func TestVerifyLoginSucceedsAndNeverReadsKV(t *testing.T) {
	t.Parallel()
	mock := newMockVault(t)
	client, err := New(mock.config())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.VerifyLogin(context.Background()); err != nil {
		t.Fatalf("VerifyLogin() = %v", err)
	}
	if mock.logins.Load() != 1 || mock.revokes.Load() != 1 {
		t.Fatalf("logins=%d revokes=%d, want 1/1", mock.logins.Load(), mock.revokes.Load())
	}
	// The probe must never touch KV or transit — a timer-driven probe must not
	// cycle release credentials through server heap to light a dashboard LED.
	if mock.reads.Load() != 0 {
		t.Fatalf("VerifyLogin touched KV: reads=%d", mock.reads.Load())
	}
}

func TestVerifyLoginFailsWithBadRole(t *testing.T) {
	t.Parallel()
	mock := newMockVault(t)
	config := mock.config()
	config.Role = "wrong-role"
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.VerifyLogin(context.Background()); err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("VerifyLogin() with bad role = %v, want login failure", err)
	}
	if mock.badLogins.Load() != 1 {
		t.Fatalf("badLogins=%d, want 1", mock.badLogins.Load())
	}
	if mock.reads.Load() != 0 {
		t.Fatal("failed VerifyLogin must not read any KV path")
	}
}

func TestVerifyLoginFailsWhenStoreIsSealed(t *testing.T) {
	t.Parallel()
	sealed := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte(`{"errors":["Vault is sealed"]}`))
	}))
	defer sealed.Close()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: sealed.Certificate().Raw})
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte(testServiceJWT), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{
		Address: sealed.URL, Role: "oberth", CACertPEM: caPEM,
		ServiceAccountTokenPath: tokenPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.VerifyLogin(context.Background()); err == nil {
		t.Fatal("VerifyLogin must fail when the store is sealed (503)")
	}
}

func TestVerifyLoginFailsWithTLSError(t *testing.T) {
	t.Parallel()
	mock := newMockVault(t)
	config := mock.config()
	// Generate a fresh self-signed CA that does not sign the mock's TLS
	// certificate. The client trusts only this wrong CA, so the TLS
	// handshake must fail.
	wrongKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}, &x509.Certificate{
		SerialNumber: big.NewInt(1),
	}, &wrongKey.PublicKey, wrongKey)
	if err != nil {
		t.Fatal(err)
	}
	config.CACertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: wrongDER})
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.VerifyLogin(context.Background()); err == nil {
		t.Fatal("VerifyLogin must fail with TLS certificate validation failure")
	}
}

func TestSealStatusDetectsSealed(t *testing.T) {
	t.Parallel()
	mock := newMockVault(t)
	mock.sealed.Store(true)
	client, err := New(mock.config())
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := client.SealStatus(context.Background())
	if err != nil {
		t.Fatalf("SealStatus() error = %v", err)
	}
	if !sealed {
		t.Fatal("SealStatus() = false, want true when store is sealed")
	}
	if mock.sealChecks.Load() != 1 {
		t.Fatalf("sealChecks = %d, want 1", mock.sealChecks.Load())
	}
}

func TestSealStatusDetectsUnsealed(t *testing.T) {
	t.Parallel()
	mock := newMockVault(t)
	// sealed is false by default
	client, err := New(mock.config())
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := client.SealStatus(context.Background())
	if err != nil {
		t.Fatalf("SealStatus() error = %v", err)
	}
	if sealed {
		t.Fatal("SealStatus() = true, want false when store is unsealed")
	}
}

func TestSealStatusReportsNetworkError(t *testing.T) {
	t.Parallel()
	mock := newMockVault(t)
	client, err := New(mock.config())
	if err != nil {
		t.Fatal(err)
	}
	mock.Close()
	_, err = client.SealStatus(context.Background())
	if err == nil {
		t.Fatal("SealStatus() must fail when the store is unreachable")
	}
}

func TestSealStatusSendsNoAuthCredential(t *testing.T) {
	t.Parallel()
	var observedToken atomic.Value
	observedToken.Store("")
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if token := request.Header.Get("X-Vault-Token"); token != "" {
			observedToken.Store(token)
		}
		if auth := request.Header.Get("Authorization"); auth != "" {
			observedToken.Store(auth)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"type": "shamir", "initialized": true, "sealed": false,
			"t": 1, "n": 1, "progress": 0,
		})
	}))
	defer server.Close()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte(testServiceJWT), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{
		Address: server.URL, Role: "oberth", CACertPEM: caPEM,
		ServiceAccountTokenPath: tokenPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := client.SealStatus(context.Background())
	if err != nil {
		t.Fatalf("SealStatus() error = %v", err)
	}
	if sealed {
		t.Fatal("expected unsealed")
	}
	if token := observedToken.Load().(string); token != "" {
		t.Fatalf("seal-status request carried credential header: %q", token)
	}
}

func assertSanitizedHTTPError(t *testing.T, err error, status int, forbidden ...string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), http.StatusText(status)) || !strings.Contains(err.Error(), "HTTP") {
		t.Fatalf("Secret-store HTTP %d error = %v", status, err)
	}
	if len(err.Error()) > 512 {
		t.Fatalf("Secret-store HTTP error retained an oversized response: %d bytes", len(err.Error()))
	}
	for _, value := range forbidden {
		if strings.Contains(err.Error(), value) {
			t.Fatalf("Secret-store HTTP error reflected sensitive value %q", value)
		}
	}
}
