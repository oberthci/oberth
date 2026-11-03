package api

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestAuthenticationFailuresAreLimitedByPeerAndCredentialAndLogged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&logs)
		defer log.SetOutput(previous)
		server, _ := testServer(t)
		lookups := 0
		server.auth = streamAuthFunc(func(context.Context, string) (Actor, error) { lookups++; return Actor{}, ErrInvalidToken })
		call := func(peer string, malformed bool) int {
			request := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
			request.RemoteAddr = peer
			if !malformed {
				request.Header.Set("Authorization", "Bearer private-test-credential")
			}
			// Forwarded headers from an untrusted client cannot reset its limit.
			request.Header.Set("X-Forwarded-For", peer)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			return response.Code
		}
		for i := 0; i < 10; i++ {
			if code := call("192.0.2.1:1234", false); code != http.StatusUnauthorized {
				t.Fatalf("failure %d: %d", i, code)
			}
		}
		if code := call("192.0.2.1:5678", false); code != http.StatusTooManyRequests {
			t.Fatalf("exhausted IP: %d, want 429", code)
		}
		if lookups != 11 {
			t.Fatalf("failure lookup count: %d lookups", lookups)
		}
		if code := call("192.0.2.2:1234", false); code != http.StatusUnauthorized {
			t.Fatalf("other IP: %d", code)
		}
		time.Sleep(time.Minute)
		if code := call("192.0.2.1:9999", false); code != http.StatusUnauthorized {
			t.Fatalf("expired limit: %d", code)
		}
		if strings.Count(logs.String(), "api: authentication failure:") != 12 || strings.Contains(logs.String(), "private-test-credential") {
			t.Fatalf("failure logging missing or includes credential: %s", logs.String())
		}
	})
}

func TestValidBearerSurvivesSharedPeerFailureLimit(t *testing.T) {
	server, _ := testServer(t)
	server.auth = streamAuthFunc(func(_ context.Context, token string) (Actor, error) {
		if token == "valid" {
			return Actor{Identity: "operator", Admin: true}, nil
		}
		return Actor{}, ErrInvalidToken
	})
	for i := 0; i < 50; i++ {
		request := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
		request.RemoteAddr = "192.0.2.1:1234"
		request.Header.Set("Authorization", "Bearer invalid")
		server.Handler().ServeHTTP(httptest.NewRecorder(), request)
	}
	for i := 0; i < 20; i++ {
		request := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
		request.RemoteAddr = "192.0.2.1:5678"
		request.Header.Set("Authorization", "Bearer valid")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("valid token after peer failures: %d", response.Code)
		}
	}
}

func TestFailureLimiterSeparatesCredentialsBehindSharedIngress(t *testing.T) {
	server, _ := testServer(t)
	server.auth = streamAuthFunc(func(context.Context, string) (Actor, error) { return Actor{}, ErrInvalidToken })
	call := func(token string) int {
		request := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
		request.RemoteAddr = "192.0.2.1:1234"
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response.Code
	}
	for range authFailureLimit {
		if code := call("invalid-one"); code != 401 {
			t.Fatalf("initial failure: %d", code)
		}
	}
	if code := call("invalid-one"); code != 429 {
		t.Fatalf("exhausted principal: %d", code)
	}
	for _, token := range []string{"invalid-two", "expired-token", ""} {
		if code := call(token); code != 401 {
			t.Fatalf("distinct credential behind ingress got %d, want 401", code)
		}
	}
}

func TestFailureLimiterCapacityDoesNotRejectNewPrincipal(t *testing.T) {
	var limiter authFailureLimiter
	for i := range maxAuthFailurePeers {
		limiter.check(fmt.Sprintf("principal-%d", i), true)
	}
	if limiter.check("new-principal", true) {
		t.Fatal("unrelated buckets must not rate-limit a new principal")
	}
	if len(limiter.peers) > maxAuthFailurePeers {
		t.Fatal("failure limiter exceeded its memory bound")
	}
}
