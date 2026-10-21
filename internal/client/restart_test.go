package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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
