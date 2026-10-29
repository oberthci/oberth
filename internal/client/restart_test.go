package client

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func TestGetReconnectsAcrossUnavailableOrTruncatedBody(t *testing.T) {
	for _, failure := range []string{"503", "truncated"} {
		t.Run(failure, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("ref") != "main" {
					t.Error("lost target")
				}
				if calls.Add(1) == 1 {
					if failure == "503" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("Content-Length", "1000")
					io.WriteString(w, `{"status":`)
					return
				}
				io.WriteString(w, `{"status":"passed"}`)
			}))
			defer server.Close()
			var response struct{ Status string }
			if err := newClient(t, server).Get(t.Context(), "/api/runs/same-run", map[string]string{"ref": "main"}, &response); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 || response.Status != "passed" {
				t.Fatalf("calls=%d result=%+v", calls.Load(), response)
			}
		})
	}
}

func TestGetReconnectDeadline(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	var result any
	if err := newClient(t, server).Get(ctx, "/api/status", nil, &result); err == nil {
		t.Fatal("missing outage error")
	}
	if calls.Load() != 1 {
		t.Fatalf("retry ignored deadline: %d", calls.Load())
	}
}

// Keep the full production retry/deadline path covered without spending thirty
// wall-clock seconds on each error-shape fixture in client_test.go.
func TestGetPersistentFailureUsesWholeRequestBudget(t *testing.T) {
	for _, failure := range []string{"gateway", "connection refused"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, err := New(t.Context(), Config{BaseURL: "https://example.invalid", Token: secret})
				if err != nil {
					t.Fatal(err)
				}
				transport := &persistentFailureTransport{failure: failure}
				client.http.Transport = transport
				started := time.Now()
				var result any
				err = client.Get(t.Context(), "/api/runs", nil, &result)
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != requestTimeout || transport.calls < 2 {
					t.Fatalf("persistent failure: err=%v elapsed=%s calls=%d", err, time.Since(started), transport.calls)
				}
				if strings.Contains(err.Error(), secret) {
					t.Fatal("retry exhaustion leaked token")
				}
			})
		})
	}
}

type persistentFailureTransport struct {
	failure string
	calls   int
}

func (transport *persistentFailureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.calls++
	if transport.failure == "connection refused" {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("<html>gateway timeout</html>")), Request: request}, nil
}
