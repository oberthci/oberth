package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oberthci/oberth/internal/client"
)

func TestRemoteRunIDIsResolvedByServer(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "list forbidden", http.StatusForbidden)
	}))
	defer server.Close()
	api, err := client.New(context.Background(), client.Config{BaseURL: server.URL, Token: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveRemoteRunID(t.Context(), api, "abcdef12")
	if err != nil || got != "abcdef12" || requests != 0 {
		t.Fatalf("ID=%q err=%v list requests=%d", got, err, requests)
	}
}
func TestRemoteRejectsTooShortRunPrefixes(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; _, _ = w.Write([]byte(`[]`)) }))
	defer server.Close()
	api, err := client.New(t.Context(), client.Config{BaseURL: server.URL, Token: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "abcdef1"} {
		if got, err := resolveRemoteRunID(t.Context(), api, id); err == nil {
			t.Errorf("accepted %q as %q", id, got)
		}
	}
	if requests != 0 {
		t.Errorf("invalid IDs made %d list requests", requests)
	}
}
