package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func guardedMCPGet(t *testing.T, client *http.Client, address string) error {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer fixture-redirect-credential")
	response, err := client.Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	return err
}

func TestMCPRedirectRefusesHTTPSDowngradeToLoopback(t *testing.T) {
	var cleartextRequests atomic.Int32
	cleartext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cleartextRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer cleartext.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cleartext.URL, http.StatusFound)
	}))
	defer secure.Close()
	client, err := mcpHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	transport.TLSClientConfig.RootCAs = secure.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	defer transport.CloseIdleConnections()
	err = guardedMCPGet(t, client, secure.URL)
	if err == nil || !strings.Contains(err.Error(), "HTTPS redirect downgrade refused") || cleartextRequests.Load() != 0 {
		t.Fatalf("downgrade result: %v, cleartext requests=%d", err, cleartextRequests.Load())
	}
}

func TestMCPRedirectRevalidatesCleartextDestination(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "http://unsafe.invalid/end", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := mcpHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	transport.Proxy = nil
	// Every possible destination stays on the fixture listener, including if
	// URL validation is removed. No external resolution or connection occurs.
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	defer transport.CloseIdleConnections()
	err = guardedMCPGet(t, client, server.URL+"/start")
	if err == nil || !strings.Contains(err.Error(), "cleartext") || requests.Load() != 1 {
		t.Fatalf("unsafe redirect result: %v, requests=%d", err, requests.Load())
	}
}

func TestMCPRedirectLimitsCredentialResends(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requests.Add(1)
		if count < 12 {
			http.Redirect(w, r, fmt.Sprintf("/hop/%d", count), http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "redirect chain ended")
	}))
	defer server.Close()
	client, err := mcpHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	err = guardedMCPGet(t, client, server.URL)
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") || requests.Load() != 10 {
		t.Fatalf("redirect limit result: %v, requests=%d", err, requests.Load())
	}
}
