package secretstore

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type mockVault struct {
	*httptest.Server
	caPEM     []byte
	tokenPath string
	logins    atomic.Int64
	revokes   atomic.Int64
	badLogins atomic.Int64
	reads     atomic.Int64
}

const testLoginToken = "s.test-login-token"
const testServiceJWT = "header.payload.signature"

func newMockVault(t *testing.T) *mockVault {
	t.Helper()
	mock := &mockVault{}
	handler := http.NewServeMux()
	handler.HandleFunc("PUT /v1/auth/kubernetes/login", func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["jwt"] != testServiceJWT || body["role"] != "oberth" {
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
	handler.HandleFunc("PUT /v1/auth/token/revoke-self", func(writer http.ResponseWriter, request *http.Request) {
		if !authenticated(writer, request) {
			return
		}
		mock.revokes.Add(1)
		writer.WriteHeader(http.StatusNoContent)
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
		{name: "deleted kv v2 entry", path: "ci/data/deleted", want: "deleted or has no data"},
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
	if _, err := New(config); err != nil {
		t.Fatalf("explicit insecure development override rejected: %v", err)
	}
}
