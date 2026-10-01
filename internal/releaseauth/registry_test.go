package releaseauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegistryDelegationIsBoundToFixedRoleAndGoogleEndpoints(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	var tokenCalls, iamCalls, otherCalls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenCalls++
			if r.Method != http.MethodPost || r.ParseForm() != nil || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || r.Form.Get("assertion") == "" {
				t.Error("source credential was not a JWT exchange")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"source-delegator-token","token_type":"Bearer","expires_in":3600}`)
		case "/v1/projects/-/serviceAccounts/oberth-release-publisher@skipopsmain.iam.gserviceaccount.com:generateAccessToken":
			iamCalls++
			if r.Header.Get("Authorization") != "Bearer source-delegator-token" {
				t.Error("IAM request lacks explicit source identity")
			}
			var body struct {
				Scope    []string `json:"scope"`
				Lifetime string   `json:"lifetime"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Scope) != 1 || body.Scope[0] != registryScope || body.Lifetime != "3600s" {
				t.Error("IAM request has unexpected scope or lifetime")
			}
			_, _ = fmt.Fprintf(w, `{"accessToken":"restricted-image-token","expireTime":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case "/v1/projects/-/serviceAccounts/oberth-chart-publisher@skipopsmain.iam.gserviceaccount.com:generateAccessToken":
			iamCalls++
			// This source's IAM binding does not grant the chart-writer role.
			http.Error(w, "DENIED_SECRET_CANARY", http.StatusForbidden)
		default:
			otherCalls++
			http.Error(w, "unexpected destination", http.StatusForbidden)
		}
	}))
	defer server.Close()
	body, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "skipopsmain",
		"client_email": "source-image-delegator@skipopsmain.iam.gserviceaccount.com",
		"private_key":  privateKey, "token_uri": server.URL + "/attacker-endpoint",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/nonexistent/ambient-credentials")
	token, err := registryToken(context.Background(), "publish-images", body, server.Client(), server.URL+"/token", server.URL+"/v1")
	if err != nil || string(token) != "restricted-image-token" {
		t.Fatalf("delegated token = %q, %v", token, err)
	}
	if tokenCalls != 1 || iamCalls != 1 || otherCalls != 0 {
		t.Fatalf("requests = token:%d IAM:%d other:%d", tokenCalls, iamCalls, otherCalls)
	}
	if _, err := registryToken(context.Background(), "publish-chart", body, server.Client(), server.URL+"/token", server.URL+"/v1"); err == nil || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("wrong-role delegation result = %v", err)
	}
	before := tokenCalls
	for _, action := range []string{"publish-website", "oberth-release-publisher@attacker.test", ""} {
		if _, err := registryToken(context.Background(), action, body, server.Client(), server.URL+"/token", server.URL+"/v1"); err == nil {
			t.Fatalf("unsupported role %q accepted", action)
		}
	}
	for _, invalid := range [][]byte{
		[]byte(`[]`), []byte(`{"type":"service_account","type":"service_account"}`),
		append(append([]byte{}, body...), []byte(` {}`)...),
		[]byte(strings.Replace(string(body), "skipopsmain", "other-project", 1)),
		[]byte(strings.Replace(string(body), "source-image-delegator@skipopsmain.iam.gserviceaccount.com", "attacker@elsewhere.test", 1)),
	} {
		if _, err := registryToken(context.Background(), "publish-images", invalid, server.Client(), server.URL+"/token", server.URL+"/v1"); err == nil {
			t.Fatal("invalid source accepted")
		}
	}
	if tokenCalls != before || otherCalls != 0 {
		t.Fatal("invalid input reached authentication or a key-selected endpoint")
	}
}
