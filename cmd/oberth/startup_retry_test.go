package main

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/secretstore"
)

func TestStartupRetriesTransientBaoAndRefusesPermanentErrors(t *testing.T) {
	for _, tc := range []struct {
		code     int
		attempts int
	}{{503, 4}, {403, 1}, {400, 1}} {
		calls := 0
		err := retryStartup(t.Context(), time.Second, time.Millisecond, func(context.Context) error {
			calls++
			if calls < 4 {
				return &secretstore.StatusError{Op: "read identity", Code: tc.code}
			}
			return nil
		}, transientIdentityStartup, nil)
		if calls != tc.attempts || (tc.code == 503) != (err == nil) {
			t.Fatalf("HTTP %d calls=%d err=%v", tc.code, calls, err)
		}
	}
}
func TestStartupRetryStopsAtBudgetAndProjectionAppears(t *testing.T) {
	calls := 0
	err := retryStartup(t.Context(), 20*time.Millisecond, time.Millisecond, func(context.Context) error { calls++; return &secretstore.StatusError{Code: 503} }, transientIdentityStartup, nil)
	if !errors.Is(err, context.DeadlineExceeded) || calls < 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	calls = 0
	err = retryStartup(t.Context(), time.Second, time.Millisecond, func(context.Context) error {
		calls++
		if calls < 3 {
			return &os.PathError{Op: "open", Path: "/var/run/secrets/kubernetes.io/serviceaccount/token", Err: os.ErrNotExist}
		}
		return nil
	}, transientIdentityStartup, nil)
	if err != nil || calls != 3 {
		t.Fatalf("projection: calls=%d err=%v", calls, err)
	}
}
func TestBootstrapHealthHasNoReadyOrApplicationRoute(t *testing.T) {
	stop, address, err := startBootstrapHealth(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: time.Second} //nolint:gosec // Ephemeral, probe-only TLS identity.
	defer client.CloseIdleConnections()
	for path, want := range map[string]int{"/livez": 200, "/healthz": 200, "/readyz": 503, "/mcp": 503} {
		response, err := client.Get("https://" + address + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != want || strings.Contains(string(body), "token") {
			t.Fatalf("%s status=%d body=%q", path, response.StatusCode, body)
		}
	}
	stop() // Closure is joined and idempotent before the real listener binds.
}

func TestStartupOpenBaoHTTPRecoversAfterThree503s(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/kubernetes/login":
			if calls.Add(1) <= 3 {
				w.WriteHeader(503)
				return
			}
			_, _ = io.WriteString(w, `{"auth":{"client_token":"test-session","lease_duration":60}}`)
		case "/v1/auth/token/revoke-self":
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := secretstore.New(secretstore.Config{Address: server.URL, Role: "test-role", CACertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), ServiceAccountTokenSource: func(context.Context) ([]byte, error) { return []byte("test-jwt"), nil }})
	if err != nil {
		t.Fatal(err)
	}
	err = retryStartup(t.Context(), 15*time.Second, time.Millisecond, func(ctx context.Context) error { _, _, err := client.ReadIdentity(ctx, "oberth", "server"); return err }, transientIdentityStartup, nil)
	if err != nil || calls.Load() != 4 {
		t.Fatalf("HTTP login calls=%d err=%v", calls.Load(), err)
	}
}
