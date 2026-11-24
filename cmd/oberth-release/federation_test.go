package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type federationTransport func(*http.Request) (*http.Response, error)

func (f federationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func checkSTSRequest(t *testing.T, r *http.Request) {
	t.Helper()
	if r.URL.String() != "https://sts.googleapis.com/v1/token" || r.Method != "POST" {
		t.Fatal("wrong exchange endpoint")
	}
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{
		"subject_token":      "projected-fixture",
		"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
		"audience":           "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/pool/providers/provider",
	} {
		if r.Form.Get(field) != want {
			t.Fatal("projected identity or provider was not exchanged", field)
		}
	}
}

func checkImpersonationRequest(t *testing.T, r *http.Request) {
	t.Helper()
	if r.URL.String() != "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/writer@project.iam.gserviceaccount.com:generateAccessToken" || r.Header.Get("Authorization") != "Bearer federated-fixture" {
		t.Fatal("account impersonation differs")
	}
	var request struct {
		Lifetime string
		Scope    []string
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Fatal(err)
	}
	if request.Lifetime != "3600s" || len(request.Scope) != 1 || request.Scope[0] != "https://www.googleapis.com/auth/cloud-platform" {
		t.Fatal("account lifetime or scope differs")
	}
}

func TestRegistryFederationExchangesProjectedIdentity(t *testing.T) {
	for _, fail := range []string{"", "sts", "impersonation"} {
		t.Run(fail, func(t *testing.T) { checkFederation(t, fail) })
	}
}

func checkFederation(t *testing.T, fail string) {
	t.Helper()
	dir := t.TempDir()
	subject := filepath.Join(dir, "subject")
	if err := os.WriteFile(subject, []byte("projected-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &http.Client{Transport: federationTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		deadline, ok := r.Context().Deadline()
		if !ok || !deadline.Before(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)) {
			t.Fatal("exchange is not bounded")
		}
		body := `{"access_token":"federated-fixture","token_type":"Bearer","expires_in":3600}`
		stage := "sts"
		switch calls {
		case 1:
			checkSTSRequest(t, r)
		case 2:
			stage = "impersonation"
			checkImpersonationRequest(t, r)
			body = `{"accessToken":"registry-fixture","expireTime":"2100-01-01T00:00:00Z"}`
		default:
			t.Fatal("unexpected exchange or retry")
		}
		status := http.StatusOK
		if stage == fail {
			status, body = http.StatusForbidden, `{"error":"projected-fixture federated-fixture"}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	ctx := context.WithValue(t.Context(), oauth2.HTTPClient, client)
	login := filepath.Join(dir, "registry")
	err := run(ctx, []string{"registry-login", "--provider", "projects/123/locations/global/workloadIdentityPools/pool/providers/provider", "--service-account", "writer@project.iam.gserviceaccount.com", "--identity-file", subject, "--dir", login})
	if fail != "" {
		want := map[string]refusal{"sts": federationExchange, "impersonation": federationImpersonation}[fail]
		if !errors.Is(err, want) {
			t.Fatal("provider response leaked or failure ignored", err)
		}
		if _, err := os.Stat(login); !os.IsNotExist(err) {
			t.Fatal("failed exchange wrote a login")
		}
		return
	}
	if err != nil || calls != 2 {
		t.Fatal("federation failed", err, calls)
	}
	assertRegistryConfig(t, login)
}

func assertRegistryConfig(t *testing.T, login string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(login, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Auths map[string]struct{ Auth string }
	}
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(config.Auths[registryHost].Auth)
	if err != nil || string(decoded) != "oauth2accesstoken:registry-fixture" || len(config.Auths) != 1 {
		t.Fatal("registry login differs")
	}
	for path, mode := range map[string]os.FileMode{login: 0700, filepath.Join(login, "config.json"): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal("login is not private", err)
		}
	}
}
