package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunPagingUsesServerCursor(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("before") == "opaque-cursor" {
			_, _ = w.Write([]byte(`[{"ID":"older","Status":"passed","QueuedAt":"2026-01-01T00:00:00Z"}]`))
		} else {
			w.Header().Set("X-Oberth-Next-Before", "opaque-cursor")
			_, _ = w.Write([]byte(`[{"ID":"newer","Status":"passed","QueuedAt":"2026-01-02T00:00:00Z"}]`))
		}
	}))
	defer server.Close()
	configure(t, server)
	var out bytes.Buffer
	if err := runRuns(t.Context(), []string{"--all", "--limit", "2"}, &out); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !strings.Contains(out.String(), "older") {
		t.Fatalf("requests=%d output=%s", requests, out.String())
	}
}
