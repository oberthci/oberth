package api

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestAuthenticationFailuresAreLimitedByPeerIPAndLogged(t *testing.T) {
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
			if code := call("192.0.2.1:1234", i%2 == 0); code != http.StatusUnauthorized {
				t.Fatalf("failure %d: %d", i, code)
			}
		}
		if code := call("192.0.2.1:5678", false); code != http.StatusTooManyRequests {
			t.Fatalf("exhausted IP: %d, want 429", code)
		}
		if lookups != 5 {
			t.Fatalf("throttled request reached authenticator: %d lookups", lookups)
		}
		if code := call("192.0.2.2:1234", false); code != http.StatusUnauthorized {
			t.Fatalf("other IP: %d", code)
		}
		time.Sleep(time.Minute)
		if code := call("192.0.2.1:9999", false); code != http.StatusUnauthorized {
			t.Fatalf("expired limit: %d", code)
		}
		if strings.Count(logs.String(), "api: authentication failure:") != 13 || strings.Contains(logs.String(), "private-test-credential") {
			t.Fatalf("failure logging missing or includes credential: %s", logs.String())
		}
	})
}
