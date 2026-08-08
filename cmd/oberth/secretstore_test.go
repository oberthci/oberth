package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	oberth "github.com/oberthci/oberth"
)

func writeServeCmdline(t *testing.T, argv ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cmdline")
	if err := os.WriteFile(path, []byte(strings.Join(argv, "\x00")+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSecretStoreCmdlineDiscovery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		argv []string
		want secretStoreVerifyConfig
	}{
		{
			name: "equals form",
			argv: []string{
				"/usr/local/bin/oberth", "serve", "--namespace=oberth",
				"--secretstore-address=https://store.example:8200",
				"--secretstore-role=oberth-ci",
				"--secretstore-path=oberth/data/one",
				"--secretstore-path=oberth/data/two",
			},
			want: secretStoreVerifyConfig{
				address: "https://store.example:8200", role: "oberth-ci",
				paths: []string{"oberth/data/one", "oberth/data/two"},
			},
		},
		{
			name: "space and single-dash forms with unrelated value flags",
			argv: []string{
				"oberth", "serve", "-runner-image", "img@sha256:abc", "--job-ttl", "600",
				"-secretstore-address", "https://store.example:8200",
				"--secretstore-k8s-auth-mount", "k8s-prod",
				"--secretstore-role", "oberth-ci",
				"--secretstore-ca-cert", "/etc/oberth/secretstore-ca/ca.crt",
				"--secretstore-sa-token", "/var/run/secrets/tokens/store",
				"--secretstore-insecure-http=false",
			},
			want: secretStoreVerifyConfig{
				address: "https://store.example:8200", authMount: "k8s-prod", role: "oberth-ci",
				caCertPath: "/etc/oberth/secretstore-ca/ca.crt", saTokenPath: "/var/run/secrets/tokens/store",
			},
		},
		{
			name: "bare insecure boolean",
			argv: []string{
				"oberth", "serve",
				"--secretstore-address=http://127.0.0.1:8200", "--secretstore-insecure-http",
				"--secretstore-role=dev",
			},
			want: secretStoreVerifyConfig{address: "http://127.0.0.1:8200", role: "dev", insecureHTTP: true},
		},
		{
			name: "no secret store flags",
			argv: []string{"oberth", "serve", "--namespace=oberth"},
			want: secretStoreVerifyConfig{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := secretStoreConfigFromServeCmdline(writeServeCmdline(t, test.argv...))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("discovered = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestSecretStoreCmdlineDiscoveryRejectsForeignProcesses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "not oberth", argv: []string{"/bin/sh", "serve"}, want: "not an oberth serve process"},
		{name: "not serve", argv: []string{"oberth", "upstream"}, want: "not an oberth serve process"},
		{name: "dangling value flag", argv: []string{"oberth", "serve", "--secretstore-address"}, want: "has no value"},
		{name: "invalid insecure value", argv: []string{"oberth", "serve", "--secretstore-insecure-http=maybe"}, want: "invalid value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := secretStoreConfigFromServeCmdline(writeServeCmdline(t, test.argv...))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	if _, err := secretStoreConfigFromServeCmdline(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing cmdline file must fail discovery")
	}
}

// TestSecretStoreCmdlineMatchesServeParsing pins the tolerant cmdline scanner
// to the authoritative serve flag definitions: if a --secretstore-* flag is
// renamed or re-typed in serve.go without updating the verifier, this fails.
func TestSecretStoreCmdlineMatchesServeParsing(t *testing.T) {
	t.Parallel()
	arguments := []string{
		"--runner-image=registry.example/oberth-ci@sha256:" + strings.Repeat("a", 64),
		"--secretstore-address=https://store.example:8200",
		"--secretstore-k8s-auth-mount=k8s-prod",
		"--secretstore-role=oberth-ci",
		"--secretstore-ca-cert=/etc/oberth/secretstore-ca/ca.crt",
		"--secretstore-sa-token=/var/run/secrets/tokens/store",
		"--secretstore-path=oberth/data/one",
		"--secretstore-path=oberth/data/two",
	}
	options, err := parseServeOptions(arguments)
	if err != nil {
		t.Fatalf("serve rejected the reference secret store arguments: %v", err)
	}
	discovered, err := secretStoreConfigFromServeCmdline(writeServeCmdline(t, append([]string{"oberth", "serve"}, arguments...)...))
	if err != nil {
		t.Fatal(err)
	}
	want := secretStoreVerifyConfig{
		address:      options.secretStoreAddress,
		authMount:    options.secretStoreAuthMount,
		role:         options.secretStoreRole,
		caCertPath:   options.secretStoreCACert,
		saTokenPath:  options.secretStoreSAToken,
		insecureHTTP: options.secretStoreInsecureHTTP,
		paths:        options.secretStorePaths,
	}
	if !reflect.DeepEqual(discovered, want) {
		t.Fatalf("discovered = %+v, want the serve-parsed configuration %+v", discovered, want)
	}
}

func TestSecretStoreSetupGuidanceAndScript(t *testing.T) {
	t.Parallel()
	var script bytes.Buffer
	if err := runSecretStoreSetup([]string{"--print-script"}, &script); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(script.Bytes(), oberth.SetupSecretStoreScript) {
		t.Fatal("--print-script must emit exactly the embedded setup script")
	}
	if !bytes.HasPrefix(script.Bytes(), []byte("#!")) || !bytes.Contains(script.Bytes(), []byte("setup-secretstore")) {
		t.Fatalf("embedded setup script looks wrong: %q", script.Bytes()[:60])
	}
	var guidance bytes.Buffer
	if err := runSecretStoreSetup(nil, &guidance); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(oberth.SetupSecretStoreScript)
	for _, want := range []string{
		hex.EncodeToString(digest[:]),
		"--print-script",
		"oberth secretstore verify",
		"no code path that accepts a store admin token",
	} {
		if !strings.Contains(guidance.String(), want) {
			t.Fatalf("setup guidance misses %q:\n%s", want, guidance.String())
		}
	}
	if err := runSecretStoreSetup([]string{"unexpected"}, &guidance); err == nil {
		t.Fatal("positional arguments must be rejected")
	}
}

type mockStore struct {
	*httptest.Server
	caPath    string
	tokenPath string
	logins    atomic.Int64
	revokes   atomic.Int64
}

const mockStoreLoginToken = "s.verify-login-token"
const mockStoreJWT = "header.payload.signature"

func newMockStore(t *testing.T) *mockStore {
	t.Helper()
	mock := &mockStore{}
	handler := http.NewServeMux()
	handler.HandleFunc("PUT /v1/auth/kubernetes/login", func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["jwt"] != mockStoreJWT || body["role"] != "oberth-ci" {
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(`{"errors":["invalid role or jwt"]}`))
			return
		}
		mock.logins.Add(1)
		_, _ = writer.Write([]byte(`{"auth":{"client_token":"` + mockStoreLoginToken + `","lease_duration":60}}`))
	})
	handler.HandleFunc("GET /v1/oberth/data/r2-upload", func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Vault-Token") != mockStoreLoginToken {
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(`{"errors":["permission denied"]}`))
			return
		}
		_, _ = writer.Write([]byte(`{"data":{"data":{"access":"r2-access","secret":"r2-secret"},"metadata":{"version":1}}}`))
	})
	handler.HandleFunc("PUT /v1/auth/token/revoke-self", func(writer http.ResponseWriter, request *http.Request) {
		mock.revokes.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	})
	handler.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"errors":[]}`))
	})
	mock.Server = httptest.NewTLSServer(handler)
	t.Cleanup(mock.Close)
	directory := t.TempDir()
	mock.caPath = filepath.Join(directory, "ca.crt")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: mock.Server.Certificate().Raw})
	if err := os.WriteFile(mock.caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	mock.tokenPath = filepath.Join(directory, "token")
	if err := os.WriteFile(mock.tokenPath, []byte(mockStoreJWT+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return mock
}

func (mock *mockStore) serveCmdline(t *testing.T, extra ...string) string {
	t.Helper()
	argv := append([]string{
		"/usr/local/bin/oberth", "serve",
		"--secretstore-address=" + mock.URL,
		"--secretstore-role=oberth-ci",
		"--secretstore-ca-cert=" + mock.caPath,
		"--secretstore-sa-token=" + mock.tokenPath,
	}, extra...)
	return writeServeCmdline(t, argv...)
}

func TestSecretStoreVerifyProvesTheDiscoveredConfiguration(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	cmdline := mock.serveCmdline(t, "--secretstore-path=oberth/data/r2-upload")
	var output bytes.Buffer
	if err := runSecretStoreVerify(context.Background(), nil, &output, cmdline); err != nil {
		t.Fatalf("verify failed: %v\n%s", err, output.String())
	}
	for _, want := range []string{
		"role=oberth-ci",
		"ok oberth/data/r2-upload (2 keys)",
		"secret store verify: OK",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("verify output misses %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "r2-secret") || strings.Contains(output.String(), "r2-access") {
		t.Fatalf("verify output must never contain secret values:\n%s", output.String())
	}
	if mock.logins.Load() != 1 || mock.revokes.Load() != 1 {
		t.Fatalf("verify must log in once and revoke its token: logins=%d revokes=%d", mock.logins.Load(), mock.revokes.Load())
	}
}

func TestSecretStoreVerifyAcceptsCandidatePathArguments(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	cmdline := mock.serveCmdline(t) // no allowlisted paths on the server yet
	var output bytes.Buffer
	if err := runSecretStoreVerify(context.Background(), []string{"oberth/data/r2-upload"}, &output, cmdline); err != nil {
		t.Fatalf("verify failed: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "ok oberth/data/r2-upload (2 keys)") {
		t.Fatalf("candidate path was not verified:\n%s", output.String())
	}
}

func TestSecretStoreVerifyFailsClosed(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	t.Run("empty allowlist and no arguments", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		err := runSecretStoreVerify(context.Background(), nil, &output, mock.serveCmdline(t))
		if err == nil || !strings.Contains(err.Error(), "nothing to verify") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing path fails with the store error and hints", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		err := runSecretStoreVerify(context.Background(), []string{"oberth/data/absent"}, &output, mock.serveCmdline(t))
		if err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("error = %v", err)
		}
		if !strings.Contains(output.String(), "common causes") {
			t.Fatalf("failure hints missing:\n%s", output.String())
		}
	})
	t.Run("wrong role fails at login", func(t *testing.T) {
		t.Parallel()
		cmdline := writeServeCmdline(t,
			"oberth", "serve",
			"--secretstore-address="+mock.URL,
			"--secretstore-role=wrong-role",
			"--secretstore-ca-cert="+mock.caPath,
			"--secretstore-sa-token="+mock.tokenPath,
			"--secretstore-path=oberth/data/r2-upload",
		)
		var output bytes.Buffer
		err := runSecretStoreVerify(context.Background(), nil, &output, cmdline)
		if err == nil || !strings.Contains(err.Error(), "login") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("invalid path syntax", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		err := runSecretStoreVerify(context.Background(), []string{"../escape"}, &output, mock.serveCmdline(t))
		if err == nil || !strings.Contains(err.Error(), "clean non-empty segments") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("unconfigured server", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		err := runSecretStoreVerify(context.Background(), nil, &output, writeServeCmdline(t, "oberth", "serve"))
		if err == nil || !strings.Contains(err.Error(), "no secret store configured") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("explicit flags require address", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		err := runSecretStoreVerify(context.Background(), []string{"--role=oberth-ci"}, &output, writeServeCmdline(t, "oberth", "serve"))
		if err == nil || !strings.Contains(err.Error(), "require --address") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestSecretStoreVerifyExplicitFlagsBypassDiscovery(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	arguments := []string{
		"--address=" + mock.URL,
		"--role=oberth-ci",
		"--ca-cert=" + mock.caPath,
		"--sa-token=" + mock.tokenPath,
		"oberth/data/r2-upload",
	}
	var output bytes.Buffer
	// The /proc path is intentionally nonexistent: explicit mode must never read it.
	if err := runSecretStoreVerify(context.Background(), arguments, &output, filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("explicit verify failed: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "secret store verify: OK") {
		t.Fatalf("verify did not succeed:\n%s", output.String())
	}
}

func TestRunCLIRoutesSecretStore(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runCLI(context.Background(), []string{"secretstore"}, strings.NewReader(""), &output)
	if err == nil || !strings.Contains(err.Error(), "secretstore setup|verify") {
		t.Fatalf("error = %v", err)
	}
	err = runCLI(context.Background(), []string{"secretstore", "unknown"}, strings.NewReader(""), &output)
	if err == nil || !strings.Contains(err.Error(), "unknown secretstore command") {
		t.Fatalf("error = %v", err)
	}
	if err := runCLI(context.Background(), []string{"secretstore", "setup"}, strings.NewReader(""), &output); err != nil {
		t.Fatalf("setup guidance failed: %v", err)
	}
}
