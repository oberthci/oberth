package secretstore

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

func TestPKIIssuanceReportsBoundedRoleDiagnostic(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/kubernetes/login":
			_, _ = io.WriteString(w, `{"auth":{"client_token":"test-session","lease_duration":60}}`)
		case "/v1/auth/token/revoke-self":
			w.WriteHeader(http.StatusNoContent)
		case "/v1/oberth-pki/issue/oberth-server":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"errors":["name ci.example.com not allowed by this role"]}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := New(Config{Address: server.URL, Role: "test-role", CACertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), ServiceAccountTokenSource: func(context.Context) ([]byte, error) { return []byte("test-jwt"), nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.IssueServerCertificate(t.Context(), ServerCertificateRequest{Mount: "oberth-pki", Role: "oberth-server", DNSNames: []string{"ci.example.com"}})
	for _, part := range []string{"name ci.example.com not allowed", "oberth-server", "ci.example.com", "bao read oberth-pki/roles/oberth-server"} {
		if err == nil || !strings.Contains(err.Error(), part) {
			t.Errorf("issuance error %v lacks %q", err, part)
		}
	}
	var status *StatusError
	if !errors.As(err, &status) || status.Code != http.StatusBadRequest || status.Op != "server certificate issuance" {
		t.Fatalf("issuance must wrap typed status: %v", err)
	}
}

func TestSecretStoreTransportErrorClasses(t *testing.T) {
	for name, cause := range map[string]error{
		"connection refused": &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		"unknown CA":         x509.UnknownAuthorityError{},
		"timeout":            &net.DNSError{IsTimeout: true},
	} {
		if err := sanitizeSecretStoreHTTPError("test", cause); !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPKIErrorBodyLimitAndEndpointIsolation(t *testing.T) {
	for _, path := range []string{"/v1/oberth-pki/issue/server", "/v1/secrets/data/value", "/v1/auth/kubernetes/login"} {
		body := &trackedResponseBody{Reader: strings.NewReader(`{"errors":["` + strings.Repeat("reflected", 1024) + `"]}`)}
		transport := boundedSecretStoreTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 400, Header: make(http.Header), Body: body}, nil
		})}
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://store.example"+path, nil)
		response, err := transport.RoundTrip(request)
		if response != nil {
			data, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if len(data) != 0 {
				t.Errorf("raw error body escaped on %s", path)
			}
		}
		if err != nil && strings.Contains(err.Error(), "reflected") {
			t.Errorf("oversized or non-PKI body escaped on %s", path)
		}
		if !body.closed {
			t.Error("error body not closed")
		}
	}
}

func TestSanitizedTransportPreservesTransientClassification(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  bool
	}{
		{syscall.ECONNREFUSED, true}, {context.DeadlineExceeded, true}, {x509.UnknownAuthorityError{}, false}, {&net.DNSError{IsTimeout: true}, true}, {errors.New("invalid request"), false},
	} {
		err := sanitizeSecretStoreHTTPError("startup", tc.cause)
		if IsUnavailable(err) != tc.want {
			t.Fatalf("%v retry=%v want=%v", err, IsUnavailable(err), tc.want)
		}
	}
}
