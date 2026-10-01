package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestR2VerificationTransportIsClosed(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "https://ambient-proxy.invalid")
	client := r2HTTPClient()
	t.Cleanup(client.CloseIdleConnections)
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS13 || client.Timeout != 30*time.Second || !errors.Is(client.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Fatal("R2 verification transport permits ambient authority or weaker TLS")
	}
	t.Setenv("OBERTH_CLOUDFLARE_API_BASE", "https://untrusted.invalid")
	if err := runR2Auth(context.Background(), []string{"r2-auth", "unused", testAccount, "oberth-releases", "oberth/", "unused"}); err == nil {
		t.Fatal("accepted arbitrary verification endpoint")
	}
}

func TestR2ExchangeRefusesUnverifiedParent(t *testing.T) {
	for _, body := range []string{
		`{"success":false,"result":{"id":"0123456789abcdef0123456789abcdef","status":"active"}}`,
		`{"success":true,"result":{"id":"0123456789abcdef0123456789abcdef","status":"expired"}}`,
		`{"success":true,"result":{"id":"wrong-id","status":"active"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			t.Cleanup(server.Close)
			got, err := exchange(context.Background(), server.Client(), server.URL, "private-token", testAccount, "oberth-releases", "oberth/")
			if err == nil || got != (credentials{}) || strings.Contains(err.Error(), "private-token") {
				t.Fatal("unverified parent emitted credentials or leaked input")
			}
		})
	}
}

func decodeR2TestClaims(t *testing.T, value credentials) map[string]any {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(value.sessionToken)
	if err != nil || !strings.HasPrefix(string(raw), "jwt/") {
		t.Fatal("invalid temporary session encoding")
	}
	parts := strings.Split(strings.TrimPrefix(string(raw), "jwt/"), ".")
	if len(parts) != 3 {
		t.Fatal("invalid compact JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal("invalid JWT payload")
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		t.Fatal("invalid JWT claims")
	}
	return claims
}

func TestLocalR2CredentialsIndependentKnownVector(t *testing.T) {
	// Calculated independently with Python hashlib/hmac and compact JSON, using
	// public fixture material. This detects hex-vs-raw HMAC keys, wrong base64,
	// scope/time/audience changes and incorrect temporary-secret derivation.
	const expectedJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJidWNrZXQiOiJvYmVydGgtcmVsZWFzZXMiLCJzY29wZSI6Im9iamVjdC1yZWFkLXdyaXRlIiwicGF0aHMiOnsicHJlZml4UGF0aHMiOlsib2JlcnRoLyJdLCJvYmplY3RQYXRocyI6W119LCJzdWIiOiIwMTIzNDU2Nzg5YWJjZGVmMDEyMzQ1Njc4OWFiY2RlZiIsImlzcyI6ImZlZGNiYTk4NzY1NDMyMTBmZWRjYmE5ODc2NTQzMjEwIiwiYXVkIjoiMDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWYucjIuY2xvdWRmbGFyZXN0b3JhZ2UuY29tIiwiaWF0IjoxNzkwODcwNDAwLCJleHAiOjE3OTA4NzIyMDB9.NqCEOJ9djB_wMaBSP_bUCzzzQH7cE-Rsp3JqrkXjIzk"
	got, err := localR2Credentials("public-test-parent-token", "fedcba9876543210fedcba9876543210", "0123456789abcdef0123456789abcdef", "oberth-releases", "oberth/", time.Unix(1790870400, 987654321))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(got.sessionToken)
	if err != nil || string(raw) != "jwt/"+expectedJWT || got.secretAccessKey != "5c0d837e84206ef3a525d0556b6b3f349e3e1b85f71d8f6e854163758748be2f" || got.accessKeyID != "fedcba9876543210fedcba9876543210" {
		t.Fatal("temporary credential differs from independent known vector")
	}
	want := map[string]any{"bucket": "oberth-releases", "scope": "object-read-write", "paths": map[string]any{"prefixPaths": []any{"oberth/"}, "objectPaths": []any{}}, "sub": "0123456789abcdef0123456789abcdef", "iss": "fedcba9876543210fedcba9876543210", "aud": "0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com", "iat": float64(1790870400), "exp": float64(1790872200)}
	if !reflect.DeepEqual(decodeR2TestClaims(t, got), want) || !validCredentials(got) {
		t.Fatal("temporary claims or output format differ")
	}
}

func TestLocalR2CredentialsRejectsInvalidScopeAndTime(t *testing.T) {
	for _, row := range []struct {
		name, token, parent, account, bucket, prefix string
		when                                         int64
	}{
		{"empty-token", "", testAccount, testAccount, "oberth-releases", "oberth/", 100},
		{"token-injection", "private\nmaterial", testAccount, testAccount, "oberth-releases", "oberth/", 100},
		{"oversized-token", strings.Repeat("p", 4097), testAccount, testAccount, "oberth-releases", "oberth/", 100},
		{"parent", "private-token", "bad", testAccount, "oberth-releases", "oberth/", 100},
		{"account", "private-token", testAccount, "bad", "oberth-releases", "oberth/", 100},
		{"bucket", "private-token", testAccount, testAccount, "other/bucket", "oberth/", 100},
		{"empty-prefix", "private-token", testAccount, testAccount, "oberth-releases", "", 100},
		{"root-prefix", "private-token", testAccount, testAccount, "oberth-releases", "/", 100},
		{"traversal", "private-token", testAccount, testAccount, "oberth-releases", "oberth/../", 100},
		{"no-slash", "private-token", testAccount, testAccount, "oberth-releases", "oberth", 100},
		{"zero-time", "private-token", testAccount, testAccount, "oberth-releases", "oberth/", 0},
		{"overflow-time", "private-token", testAccount, testAccount, "oberth-releases", "oberth/", 9223372036854775807},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, err := localR2Credentials(row.token, row.parent, row.account, row.bucket, row.prefix, time.Unix(row.when, 0))
			if err == nil || got != (credentials{}) || strings.Contains(err.Error(), "private") {
				t.Fatal("invalid input emitted credentials or leaked private input")
			}
		})
	}
}

func TestR2TokenReadBoundsAndRefusesLinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4097)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readR2Token(path); err == nil {
		t.Fatal("accepted oversized token")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{link, dir} {
		if _, err := readR2Token(name); err == nil {
			t.Fatal("accepted nonregular token input")
		}
	}
}
