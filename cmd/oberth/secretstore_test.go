package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	oberth "github.com/oberthci/oberth"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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
				"oberth", "serve", "-runner-image-prefixes", "golang:", "--job-ttl", "600",
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
		"--argo-namespace=argo-pipelines",
		"--runner-image-prefixes=golang:",
		"--secretstore-address=https://store.example:8200",
		"--secretstore-k8s-auth-mount=k8s-prod",
		"--secretstore-role=oberth-ci",
		"--secretstore-ca-cert=/etc/oberth/secretstore-ca/ca.crt",
		"--secretstore-sa-token=/var/run/secrets/tokens/store",
		"--secretstore-kv-mount=oberth-prod",
		"--secretstore-path=oberth/data/one",
		"--secretstore-path=oberth/data/two",
	}
	options, err := parseServeOptions(arguments, io.Discard)
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
		kvMount:      options.secretStoreKVMount,
		insecureHTTP: options.secretStoreInsecureHTTP,
		paths:        options.secretStorePaths,
	}
	if !reflect.DeepEqual(discovered, want) {
		t.Fatalf("discovered = %+v, want the serve-parsed configuration %+v", discovered, want)
	}
}

func TestTrustedPlanTransitServeFlagsAreNestedAndFailClosed(t *testing.T) {
	base := []string{
		"--argo-namespace=argo-pipelines",
		"--runner-image-prefixes=golang:",
		"--secretstore-address=https://store.example:8200",
		"--secretstore-role=oberth-ci",
	}
	valid := append(slices.Clone(base),
		"--secretstore-transit-mount=oberth-transit",
		"--secretstore-transit-key=trusted-plan-artifacts",
	)
	options, err := parseServeOptions(valid, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.secretStoreTransitMount != "oberth-transit" || options.secretStoreTransitKey != "trusted-plan-artifacts" {
		t.Fatalf("transit options = %+v", options)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing key", args: append(slices.Clone(base), "--secretstore-transit-mount=oberth-transit"), want: "requires both"},
		{name: "missing mount", args: append(slices.Clone(base), "--secretstore-transit-key=trusted-plan-artifacts"), want: "requires both"},
		{name: "HTTP development mode", args: []string{
			"--argo-namespace=argo-pipelines",
			"--runner-image-prefixes=golang:",
			"--secretstore-address=http://127.0.0.1:8200", "--secretstore-insecure-http", "--secretstore-role=dev",
			"--secretstore-transit-mount=oberth-transit", "--secretstore-transit-key=trusted-plan-artifacts",
		}, want: "verified HTTPS"},
		{name: "reserved mount", args: append(slices.Clone(base), "--secretstore-transit-mount=sys", "--secretstore-transit-key=trusted-plan-artifacts"), want: "reserved"},
		{name: "dot key", args: append(slices.Clone(base), "--secretstore-transit-mount=oberth-transit", "--secretstore-transit-key=.."), want: "clean non-empty"},
		{name: "nested mount", args: append(slices.Clone(base), "--secretstore-transit-mount=transit/plans", "--secretstore-transit-key=trusted-plan-artifacts"), want: "one clean"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseServeOptions(test.args, io.Discard); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
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

func TestSecretStoreBootstrapTLSWritesOnlyPublicCA(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "oberth-tls")
	var output bytes.Buffer
	err := runSecretStore(context.Background(), []string{
		"bootstrap-tls",
		"--output-dir=" + directory,
		"--namespace=openbao",
	}, &output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "BEGIN CERTIFICATE") || strings.Contains(output.String(), "PRIVATE KEY") {
		t.Fatalf("bootstrap output must contain only the public CA certificate: %q", output.String())
	}
	for _, privateName := range []string{"ca.key", "tls.key"} {
		if _, err := os.Stat(filepath.Join(directory, privateName)); err != nil {
			t.Fatalf("private material %s was not written to the PVC directory: %v", privateName, err)
		}
	}
}

// --- Release-tier verify tests ---

// newMockStoreForRole creates an HTTPS test server that accepts a specific
// OpenBao Kubernetes auth role.
func newMockStoreForRole(t *testing.T, acceptedRole string) *mockStore {
	t.Helper()
	mock := &mockStore{}
	handler := http.NewServeMux()
	handler.HandleFunc("PUT /v1/auth/kubernetes/login", func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["jwt"] != mockStoreJWT || body["role"] != acceptedRole {
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

func fakeKubeReturningToken(token string) kubeClientFactory {
	return func() (kubernetes.Interface, error) {
		fakeKube := fake.NewSimpleClientset()
		fakeKube.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if action.GetSubresource() != "token" {
				return false, nil, nil
			}
			return true, &authenticationv1.TokenRequest{
				Status: authenticationv1.TokenRequestStatus{
					Token: token,
				},
			}, nil
		})
		return fakeKube, nil
	}
}

func TestReleaseTierCmdlineDiscovery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		argv []string
		want releaseTierVerifyConfig
	}{
		{
			name: "equals form",
			argv: []string{
				"oberth", "serve",
				"--argo-namespace=oberth-argo",
				"--argo-vault-address=https://vault.example:8200",
				"--argo-vault-credentialed-role=oberth-release",
				"--argo-vault-ca-cert=/etc/oberth/argo-vault-ca/ca.crt",
				"--argo-credentialed-serviceaccount=oberth-argo-credentialed",
			},
			want: releaseTierVerifyConfig{
				vaultAddress:          "https://vault.example:8200",
				vaultCACertPath:       "/etc/oberth/argo-vault-ca/ca.crt",
				vaultCredentialedRole: "oberth-release",
				credentialedAccount:   "oberth-argo-credentialed",
				pipelineNamespace:     "oberth-argo",
			},
		},
		{
			name: "space form with unrelated flags",
			argv: []string{
				"oberth", "serve", "--namespace", "oberth", "--secretstore-address", "https://other.example:8200",
				"-argo-namespace", "oberth-argo",
				"--argo-vault-address", "https://vault.example:8200",
				"--argo-vault-credentialed-role", "oberth-release",
				"--argo-vault-ca-cert", "/etc/oberth/argo-vault-ca/ca.crt",
				"--argo-credentialed-serviceaccount", "oberth-argo-credentialed",
			},
			want: releaseTierVerifyConfig{
				vaultAddress:          "https://vault.example:8200",
				vaultCACertPath:       "/etc/oberth/argo-vault-ca/ca.crt",
				vaultCredentialedRole: "oberth-release",
				credentialedAccount:   "oberth-argo-credentialed",
				pipelineNamespace:     "oberth-argo",
			},
		},
		{
			name: "no argo flags",
			argv: []string{"oberth", "serve", "--namespace=oberth"},
			want: releaseTierVerifyConfig{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := releaseTierConfigFromServeCmdline(writeServeCmdline(t, test.argv...))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("discovered = %+v, want %+v", got, test.want)
			}
		})
	}
}

// TestReleaseTierCmdlineMatchesServeParsing pins the release-tier cmdline
// scanner to the authoritative serve flag definitions: if a --argo-* flag is
// renamed without updating the verifier, this fails.
func TestReleaseTierCmdlineMatchesServeParsing(t *testing.T) {
	t.Parallel()
	arguments := []string{
		"--argo-namespace=argo-pipelines",
		"--runner-image-prefixes=golang:",
		"--argo-vault-address=https://vault.example:8200",
		"--argo-vault-credentialed-role=oberth-release",
		"--argo-credentialed-serviceaccount=oberth-argo-credentialed",
		// --argo-vault-ca-cert is omitted: parseServeOptions reads the file at
		// the given path, which would need a real certificate in the test tree.
		// The cmdline scanner parses it identically to every other string flag;
		// TestReleaseTierCmdlineDiscovery covers that path explicitly.
	}
	options, err := parseServeOptions(arguments, io.Discard)
	if err != nil {
		t.Fatalf("serve rejected the reference release-tier arguments: %v", err)
	}
	discovered, err := releaseTierConfigFromServeCmdline(writeServeCmdline(t, append([]string{"oberth", "serve"}, arguments...)...))
	if err != nil {
		t.Fatal(err)
	}
	want := releaseTierVerifyConfig{
		vaultAddress:          options.argoVaultAddress,
		vaultCredentialedRole: options.argoVaultCredentialedRole,
		credentialedAccount:   options.argoCredentialedAccount,
		pipelineNamespace:     options.argoNamespace,
	}
	if !reflect.DeepEqual(discovered, want) {
		t.Fatalf("discovered = %+v, want the serve-parsed configuration %+v", discovered, want)
	}
}

func TestReleaseTierVerifyProvesTheDiscoveredConfiguration(t *testing.T) {
	t.Parallel()
	mock := newMockStoreForRole(t, "oberth-release")
	cmdline := writeServeCmdline(t,
		"/usr/local/bin/oberth", "serve",
		"--argo-namespace=oberth-argo",
		"--argo-credentialed-serviceaccount=oberth-argo-credentialed",
		"--argo-vault-address="+mock.URL,
		"--argo-vault-credentialed-role=oberth-release",
		"--argo-vault-ca-cert="+mock.caPath,
		"--secretstore-path=oberth/data/r2-upload",
	)
	var output bytes.Buffer
	err := runReleaseTierVerify(
		context.Background(),
		secretStoreVerifyConfig{},
		nil,
		"", "release",
		45*time.Second,
		&output,
		cmdline,
		fakeKubeReturningToken(mockStoreJWT),
	)
	if err != nil {
		t.Fatalf("release-tier verify failed: %v\n%s", err, output.String())
	}
	for _, want := range []string{
		"role=oberth-release",
		"sa=oberth-argo/oberth-argo-credentialed",
		"ok oberth/data/r2-upload (2 keys)",
		"release-tier verify: OK",
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

func TestReleaseTierVerifyAcceptsCandidatePathArguments(t *testing.T) {
	t.Parallel()
	mock := newMockStoreForRole(t, "oberth-release")
	// No --secretstore-path on the cmdline; the positional argument provides it.
	cmdline := writeServeCmdline(t,
		"/usr/local/bin/oberth", "serve",
		"--argo-namespace=oberth-argo",
		"--argo-credentialed-serviceaccount=oberth-argo-credentialed",
		"--argo-vault-address="+mock.URL,
		"--argo-vault-credentialed-role=oberth-release",
		"--argo-vault-ca-cert="+mock.caPath,
	)
	var output bytes.Buffer
	err := runReleaseTierVerify(
		context.Background(),
		secretStoreVerifyConfig{},
		[]string{"oberth/data/r2-upload"},
		"", "release",
		45*time.Second,
		&output,
		cmdline,
		fakeKubeReturningToken(mockStoreJWT),
	)
	if err != nil {
		t.Fatalf("release-tier verify failed: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "ok oberth/data/r2-upload (2 keys)") {
		t.Fatalf("candidate path was not verified:\n%s", output.String())
	}
}

func TestReleaseTierVerifyFailsClosed(t *testing.T) {
	t.Parallel()
	t.Run("rejects explicit server-tier flags", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		cmdline := writeServeCmdline(t, "oberth", "serve", "--argo-namespace=oberth-argo")
		err := runSecretStoreVerify(context.Background(), []string{
			"--release-tier", "--address=https://foo.example:8200",
		}, &output, cmdline)
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing argo-vault-address", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		cmdline := writeServeCmdline(t, "oberth", "serve",
			"--argo-namespace=oberth-argo",
			"--argo-vault-credentialed-role=oberth-release",
			"--argo-credentialed-serviceaccount=oberth-argo-credentialed",
		)
		err := runReleaseTierVerify(context.Background(), secretStoreVerifyConfig{}, nil, "", "release", 45*time.Second, &output, cmdline, fakeKubeReturningToken(""))
		if err == nil || !strings.Contains(err.Error(), "argo-vault-address") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing argo-vault-credentialed-role", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		cmdline := writeServeCmdline(t, "oberth", "serve",
			"--argo-namespace=oberth-argo",
			"--argo-vault-address=https://vault.example:8200",
			"--argo-credentialed-serviceaccount=oberth-argo-credentialed",
		)
		err := runReleaseTierVerify(context.Background(), secretStoreVerifyConfig{}, nil, "", "release", 45*time.Second, &output, cmdline, fakeKubeReturningToken(""))
		if err == nil || !strings.Contains(err.Error(), "argo-vault-credentialed-role") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing argo-credentialed-serviceaccount", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		cmdline := writeServeCmdline(t, "oberth", "serve",
			"--argo-namespace=oberth-argo",
			"--argo-vault-address=https://vault.example:8200",
			"--argo-vault-credentialed-role=oberth-release",
		)
		err := runReleaseTierVerify(context.Background(), secretStoreVerifyConfig{}, nil, "", "release", 45*time.Second, &output, cmdline, fakeKubeReturningToken(""))
		if err == nil || !strings.Contains(err.Error(), "argo-credentialed-serviceaccount") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing argo-namespace", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		cmdline := writeServeCmdline(t, "oberth", "serve",
			"--argo-vault-address=https://vault.example:8200",
			"--argo-vault-credentialed-role=oberth-release",
			"--argo-credentialed-serviceaccount=oberth-argo-credentialed",
		)
		err := runReleaseTierVerify(context.Background(), secretStoreVerifyConfig{}, nil, "", "release", 45*time.Second, &output, cmdline, fakeKubeReturningToken(""))
		if err == nil || !strings.Contains(err.Error(), "argo-namespace") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("empty paths and no arguments", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		cmdline := writeServeCmdline(t, "oberth", "serve",
			"--argo-namespace=oberth-argo",
			"--argo-vault-address=https://vault.example:8200",
			"--argo-vault-credentialed-role=oberth-release",
			"--argo-credentialed-serviceaccount=oberth-argo-credentialed",
		)
		err := runReleaseTierVerify(context.Background(), secretStoreVerifyConfig{}, nil, "", "release", 45*time.Second, &output, cmdline, fakeKubeReturningToken(""))
		if err == nil || !strings.Contains(err.Error(), "nothing to verify") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("wrong role fails at login", func(t *testing.T) {
		t.Parallel()
		mock := newMockStoreForRole(t, "oberth-release")
		cmdline := writeServeCmdline(t,
			"oberth", "serve",
			"--argo-namespace=oberth-argo",
			"--argo-credentialed-serviceaccount=oberth-argo-credentialed",
			"--argo-vault-address="+mock.URL,
			"--argo-vault-credentialed-role=wrong-role",
			"--argo-vault-ca-cert="+mock.caPath,
			"--secretstore-path=oberth/data/r2-upload",
		)
		var output bytes.Buffer
		err := runReleaseTierVerify(
			context.Background(),
			secretStoreVerifyConfig{},
			nil,
			"", "release",
			45*time.Second,
			&output,
			cmdline,
			fakeKubeReturningToken(mockStoreJWT),
		)
		if err == nil || !strings.Contains(err.Error(), "login") {
			t.Fatalf("error = %v", err)
		}
	})
}

// --- verify -keys tests ---

func TestSecretStoreVerifyKeysMatchExpectations(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	arguments := []string{
		"-keys",
		"--address=" + mock.URL,
		"--role=oberth-ci",
		"--ca-cert=" + mock.caPath,
		"--sa-token=" + mock.tokenPath,
		"--path=oberth/data/r2-upload",
		"--expect=r2-upload/access,secret",
	}
	var output bytes.Buffer
	if err := runSecretStoreVerify(context.Background(), arguments, &output, filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("verify -keys failed: %v\n%s", err, output.String())
	}
	for _, want := range []string{
		"ok oberth/data/r2-upload [access, secret]",
		"field names match expectations",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output misses %q:\n%s", want, output.String())
		}
	}
	// Values must never appear in the output.
	if strings.Contains(output.String(), "r2-secret") || strings.Contains(output.String(), "r2-access") {
		t.Fatalf("output contains secret values:\n%s", output.String())
	}
}

func TestSecretStoreVerifyKeysMissingField(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	arguments := []string{
		"-keys",
		"--address=" + mock.URL,
		"--role=oberth-ci",
		"--ca-cert=" + mock.caPath,
		"--sa-token=" + mock.tokenPath,
		"--path=oberth/data/r2-upload",
		"--expect=r2-upload/access,secret,missing_field",
	}
	var output bytes.Buffer
	err := runSecretStoreVerify(context.Background(), arguments, &output, filepath.Join(t.TempDir(), "absent"))
	if err == nil || !strings.Contains(err.Error(), "field name problem") {
		t.Fatalf("error = %v, want field name problem", err)
	}
	if !strings.Contains(output.String(), "missing fields: missing_field") {
		t.Fatalf("output should report missing field:\n%s", output.String())
	}
}

func TestSecretStoreVerifyKeysUnexpectedField(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	arguments := []string{
		"-keys",
		"--address=" + mock.URL,
		"--role=oberth-ci",
		"--ca-cert=" + mock.caPath,
		"--sa-token=" + mock.tokenPath,
		"--path=oberth/data/r2-upload",
		"--expect=r2-upload/access",
	}
	var output bytes.Buffer
	err := runSecretStoreVerify(context.Background(), arguments, &output, filepath.Join(t.TempDir(), "absent"))
	if err == nil || !strings.Contains(err.Error(), "field name problem") {
		t.Fatalf("error = %v, want field name problem", err)
	}
	if !strings.Contains(output.String(), "unexpected fields: secret") {
		t.Fatalf("output should report unexpected field:\n%s", output.String())
	}
}

func TestSecretStoreVerifyKeysListsFieldsWithoutExpect(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	arguments := []string{
		"-keys",
		"--address=" + mock.URL,
		"--role=oberth-ci",
		"--ca-cert=" + mock.caPath,
		"--sa-token=" + mock.tokenPath,
		"oberth/data/r2-upload",
	}
	var output bytes.Buffer
	if err := runSecretStoreVerify(context.Background(), arguments, &output, filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("verify -keys failed: %v\n%s", err, output.String())
	}
	for _, want := range []string{
		"ok oberth/data/r2-upload [access, secret]",
		"field names listed above",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output misses %q:\n%s", want, output.String())
		}
	}
}

func TestSecretStoreVerifyKeysInvalidExpect(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	tests := []struct {
		name   string
		expect string
		want   string
	}{
		{name: "no slash", expect: "no-slash", want: "invalid --expect"},
		{name: "empty base", expect: "/field", want: "invalid --expect"},
		{name: "empty field after slash", expect: "base/", want: "invalid --expect"},
		{name: "empty field in list", expect: "base/a,,b", want: "empty field name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			arguments := []string{
				"-keys",
				"--address=" + mock.URL,
				"--role=oberth-ci",
				"--ca-cert=" + mock.caPath,
				"--sa-token=" + mock.tokenPath,
				"--path=oberth/data/r2-upload",
				"--expect=" + test.expect,
			}
			var output bytes.Buffer
			err := runSecretStoreVerify(context.Background(), arguments, &output, filepath.Join(t.TempDir(), "absent"))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSecretStoreVerifyPathFlag(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	arguments := []string{
		"--address=" + mock.URL,
		"--role=oberth-ci",
		"--ca-cert=" + mock.caPath,
		"--sa-token=" + mock.tokenPath,
		"--path=oberth/data/r2-upload",
	}
	var output bytes.Buffer
	if err := runSecretStoreVerify(context.Background(), arguments, &output, filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("verify --path failed: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "ok oberth/data/r2-upload (2 keys)") {
		t.Fatalf("--path flag did not work:\n%s", output.String())
	}
}

func TestSecretStoreVerifyKeysExpectUnmatchedBase(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	arguments := []string{
		"-keys",
		"--address=" + mock.URL,
		"--role=oberth-ci",
		"--ca-cert=" + mock.caPath,
		"--sa-token=" + mock.tokenPath,
		"--path=oberth/data/r2-upload",
		"--expect=nonexistent-base/FIELD",
	}
	var output bytes.Buffer
	err := runSecretStoreVerify(context.Background(), arguments, &output, filepath.Join(t.TempDir(), "absent"))
	if err == nil || !strings.Contains(err.Error(), "field name problem") {
		t.Fatalf("error = %v, want field name problem for unmatched base", err)
	}
	if !strings.Contains(output.String(), "no verified path has base") {
		t.Fatalf("output should report unmatched base:\n%s", output.String())
	}
}

func TestSecretStoreVerifyKeysReleaseTierRefused(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	cmdline := writeServeCmdline(t, "oberth", "serve",
		"--argo-namespace=oberth-argo",
		"--argo-vault-address=https://vault.example:8200",
		"--argo-vault-credentialed-role=oberth-release",
		"--argo-credentialed-serviceaccount=oberth-argo-credentialed",
	)
	err := runSecretStoreVerify(context.Background(), []string{"--release-tier", "-keys"}, &output, cmdline)
	if err == nil || !strings.Contains(err.Error(), "not yet supported with --release-tier") {
		t.Fatalf("error = %v, want --release-tier rejection", err)
	}
}

func TestSecretStoreVerifyExpectImpliesKeysMode(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	// Pass --expect without -keys; should still show field names and verify.
	arguments := []string{
		"--address=" + mock.URL,
		"--role=oberth-ci",
		"--ca-cert=" + mock.caPath,
		"--sa-token=" + mock.tokenPath,
		"--path=oberth/data/r2-upload",
		"--expect=r2-upload/access,secret",
	}
	var output bytes.Buffer
	if err := runSecretStoreVerify(context.Background(), arguments, &output, filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("verify with --expect failed: %v\n%s", err, output.String())
	}
	// The output should use the field-names format, not the key-count format.
	if !strings.Contains(output.String(), "[access, secret]") {
		t.Fatalf("--expect without -keys should still list field names:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "field names match expectations") {
		t.Fatalf("output misses success message:\n%s", output.String())
	}
}
