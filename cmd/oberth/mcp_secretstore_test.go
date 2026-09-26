package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/installer"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func verifyServerOptions(mock *mockStore) serveOptions {
	return serveOptions{
		secretStoreAddress: mock.URL, secretStoreRole: "oberth-ci",
		secretStoreCACert: mock.caPath, secretStoreSAToken: mock.tokenPath,
		secretStorePaths: []string{"oberth/data/r2-upload"},
	}
}

func assertValueFreeVerification(t *testing.T, result api.SecretStoreVerifyResponse) {
	t.Helper()
	for _, secret := range []string{mockStoreJWT, mockStoreLoginToken, "r2-secret", "r2-access"} {
		if strings.Contains(result.Output, secret) {
			t.Fatal("verification output contains a credential or secret value")
		}
	}
	if len(result.Output) > maxSecretStoreVerifyOutput {
		t.Fatal("verification output exceeded its bound")
	}
}

func TestMCPSecretStoreVerifyServer(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	verify := buildSecretStoreVerifier(verifyServerOptions(mock), nil)
	for _, tc := range []struct {
		name     string
		request  api.SecretStoreVerifyRequest
		verified bool
		want     string
	}{
		{name: "configured defaults", request: api.SecretStoreVerifyRequest{}, verified: true, want: "ok oberth/data/r2-upload (2 keys)"},
		{name: "expected fields", request: api.SecretStoreVerifyRequest{Expect: []string{"r2-upload/access,secret"}}, verified: true, want: "field names match expectations"},
		{name: "missing expected field", request: api.SecretStoreVerifyRequest{Expect: []string{"r2-upload/access,missing"}}, want: "missing fields: missing"},
		{name: "candidate missing", request: api.SecretStoreVerifyRequest{Paths: []string{"oberth/data/missing"}}, want: "not found"},
		{name: "path beginning with dash", request: api.SecretStoreVerifyRequest{Paths: []string{"-role/test"}}, want: "secret store entry \"-role/test\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.request.Timeout = 5
			result, err := verify(context.Background(), tc.request)
			if err != nil || result.Verified != tc.verified || !strings.Contains(result.Output, tc.want) {
				t.Fatalf("result=%+v error=%v; want verified=%v containing %q", result, err, tc.verified, tc.want)
			}
			assertValueFreeVerification(t, result)
		})
	}
	if mock.logins.Load() != mock.revokes.Load() {
		t.Fatal("verification did not revoke every successful Vault login")
	}
}

func TestMCPSecretStoreVerifyReleaseIdentitiesStayInMemory(t *testing.T) {
	for _, tier := range []string{"shared", "release", "ci"} {
		t.Run(tier, func(t *testing.T) {
			role := "oberth-release"
			account := "shared-release"
			repo := ""
			if tier != "shared" {
				repo = "github/acme/project"
				account = installer.PerRepoName("github", "acme", "project")
				if tier == "ci" {
					account = installer.PerRepoCIName("github", "acme", "project")
				}
				role = account
			}
			mock := newMockStoreForRole(t, role)
			kube := fake.NewSimpleClientset()
			kube.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
				create, ok := action.(k8stesting.CreateActionImpl)
				if !ok || create.Namespace != "pipeline" || create.Name != account || create.Subresource != "token" {
					t.Errorf("TokenRequest used the wrong configured identity: %#v", action)
				}
				request := create.GetObject().(*authenticationv1.TokenRequest)
				if request.Spec.ExpirationSeconds == nil || *request.Spec.ExpirationSeconds != 900 {
					t.Error("TokenRequest is not short-lived")
				}
				return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: mockStoreJWT}}, nil
			})
			options := serveOptions{
				argoVaultAddress: mock.URL, argoVaultCACert: mock.caPath,
				argoVaultCredentialedRole: "oberth-release", argoCredentialedAccount: "shared-release", argoNamespace: "pipeline",
				secretStorePaths: []string{"oberth/data/r2-upload"},
			}
			blockedTemp := filepath.Join(t.TempDir(), "not-a-directory")
			if err := os.WriteFile(blockedTemp, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", blockedTemp)
			request := api.SecretStoreVerifyRequest{ReleaseTier: true, Repo: repo, Timeout: 5}
			if tier != "shared" {
				request.Tier = tier
			}
			result, err := buildSecretStoreVerifier(options, kube)(context.Background(), request)
			if err != nil || !result.Verified || !strings.Contains(result.Output, "role="+role) || !strings.Contains(result.Output, "sa=pipeline/"+account) {
				t.Fatalf("memory-only per-tier verification failed: result=%+v error=%v", result, err)
			}
			if len(kube.Actions()) != 1 || mock.logins.Load() != 1 || mock.revokes.Load() != 1 {
				t.Fatal("verification must request one identity, log in, read, and revoke")
			}
			assertValueFreeVerification(t, result)
		})
	}
}

func TestMCPSecretStoreVerifyTokenRequestErrorIsSanitized(t *testing.T) {
	t.Parallel()
	mock := newMockStoreForRole(t, "oberth-release")
	kube := fake.NewSimpleClientset()
	const reflected = "reflected-sensitive-jwt"
	kube.PrependReactor("create", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, &apierrors.StatusError{ErrStatus: metav1.Status{Code: 403, Message: reflected, Reason: metav1.StatusReason(reflected)}}
	})
	options := serveOptions{argoVaultAddress: mock.URL, argoVaultCACert: mock.caPath, argoVaultCredentialedRole: "oberth-release", argoCredentialedAccount: "release", argoNamespace: "pipeline"}
	result, err := buildSecretStoreVerifier(options, kube)(context.Background(), api.SecretStoreVerifyRequest{ReleaseTier: true, Paths: []string{"oberth/data/r2-upload"}, Timeout: 5})
	if err != nil || result.Verified || !strings.Contains(result.Output, "HTTP 403 Forbidden") || strings.Contains(result.Output, reflected) || mock.logins.Load() != 0 {
		t.Fatalf("unsafe TokenRequest failure: result=%+v error=%v", result, err)
	}
}

func TestMCPSecretStoreVerifyDeadlineIncludesTokenRequest(t *testing.T) {
	t.Parallel()
	mock := newMockStoreForRole(t, "oberth-release")
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	kubeAPI := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		entered <- struct{}{}
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	defer kubeAPI.Close()
	defer close(release)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: kubeAPI.Certificate().Raw})
	kube, err := kubernetes.NewForConfig(&rest.Config{Host: kubeAPI.URL, TLSClientConfig: rest.TLSClientConfig{CAData: ca}})
	if err != nil {
		t.Fatal(err)
	}
	options := serveOptions{argoVaultAddress: mock.URL, argoVaultCACert: mock.caPath, argoVaultCredentialedRole: "oberth-release", argoCredentialedAccount: "release", argoNamespace: "pipeline"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = buildSecretStoreVerifier(options, kube)(ctx, api.SecretStoreVerifyRequest{ReleaseTier: true, Paths: []string{"oberth/data/r2-upload"}, Timeout: 5})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("TokenRequest ignored parent deadline: %v", err)
	}
	if len(entered) != 1 || mock.logins.Load() != 0 {
		t.Fatal("deadline test did not cancel during TokenRequest before Vault login")
	}
}

func TestMCPSecretStoreVerifyDoesNotMutateConfiguredPaths(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	const declared = "oberth/upstream/acme/project/signing"
	mock.Config.Handler.(*http.ServeMux).HandleFunc("GET /v1/custom/data/upstream/acme/project/signing", func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"data":{"data":{"key":"value-not-printed"},"metadata":{"version":1}}}`))
	})
	options := verifyServerOptions(mock)
	options.secretStoreKVMount = "custom"
	options.secretStorePaths = []string{declared}
	verify := buildSecretStoreVerifier(options, nil)
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			result, err := verify(context.Background(), api.SecretStoreVerifyRequest{Timeout: 5})
			if err != nil || !result.Verified || !strings.Contains(result.Output, declared+" -> custom/data/upstream/acme/project/signing") || strings.Contains(result.Output, "value-not-printed") {
				t.Errorf("concurrent verification lost canonical path or secrecy: %+v, %v", result, err)
			}
		})
	}
	group.Wait()
	if options.secretStorePaths[0] != declared {
		t.Fatal("verification mutated live configuration")
	}
}

func TestMCPSecretStoreVerifyOutputOverflowFails(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	mock.Config.Handler.(*http.ServeMux).HandleFunc("GET /v1/oberth/data/large-keys", func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{"data": map[string]string{strings.Repeat("k", maxSecretStoreVerifyOutput): "secret-value"}, "metadata": map[string]int{"version": 1}}})
	})
	result, err := buildSecretStoreVerifier(verifyServerOptions(mock), nil)(context.Background(), api.SecretStoreVerifyRequest{Paths: []string{"oberth/data/large-keys"}, Keys: true, Timeout: 5})
	if err != nil || result.Verified || !strings.Contains(result.Output, "exceeded 64 KiB") || strings.Contains(result.Output, "secret-value") {
		t.Fatalf("overflow was not a bounded failure: %+v, %v", result, err)
	}
	if mock.revokes.Load() != 1 {
		t.Fatal("overflow left a Vault login unrevoked")
	}
}

func TestMCPSecretStoreVerifyFailsClosedOnConfigurationAndTrust(t *testing.T) {
	t.Parallel()
	mock := newMockStore(t)
	invalidCA := filepath.Join(t.TempDir(), "invalid-ca.pem")
	if err := os.WriteFile(invalidCA, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"unconfigured", "invalid CA", "wrong role"} {
		t.Run(kind, func(t *testing.T) {
			options := verifyServerOptions(mock)
			switch kind {
			case "unconfigured":
				options.secretStoreAddress = ""
			case "invalid CA":
				options.secretStoreCACert = invalidCA
			case "wrong role":
				options.secretStoreRole = "incorrect-role"
			}
			result, err := buildSecretStoreVerifier(options, nil)(context.Background(), api.SecretStoreVerifyRequest{Timeout: 5})
			if err != nil || result.Verified {
				t.Fatalf("%s accepted: %+v, %v", kind, result, err)
			}
			assertValueFreeVerification(t, result)
		})
	}
}
