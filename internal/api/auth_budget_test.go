package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestAuthenticationFailuresAreClassified(t *testing.T) {
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	for _, tc := range []struct {
		name   string
		err    error
		status int
		logged bool
	}{
		{"invalid", fmt.Errorf("wrapped: %w", ErrInvalidToken), http.StatusUnauthorized, false},
		{"read-error", errors.New("private database failure"), http.StatusServiceUnavailable, true},
		{"deadline", context.DeadlineExceeded, http.StatusServiceUnavailable, false},
		{"canceled", context.Canceled, http.StatusServiceUnavailable, false},
		{"wrapped-deadline", fmt.Errorf("lookup: %w", context.DeadlineExceeded), http.StatusServiceUnavailable, false},
		{"wrapped-canceled", fmt.Errorf("lookup: %w", context.Canceled), http.StatusServiceUnavailable, false},
		{"missing-identity", nil, http.StatusServiceUnavailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			server, _ := testServer(t)
			server.auth = streamAuthFunc(func(context.Context, string) (Actor, error) { return Actor{}, tc.err })
			request := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
			request.Header.Set("Authorization", "Bearer valid-token")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if tc.logged {
				if !strings.Contains(logs.String(), "api: authentication failure: "+tc.err.Error()) {
					t.Fatalf("authentication failure cause missing from server log: %s", &logs)
				}
			} else if logs.Len() != 0 {
				t.Fatalf("unexpected authentication log: %s", &logs)
			}
			if strings.Contains(logs.String(), "valid-token") {
				t.Fatalf("bearer token leaked into server log: %s", &logs)
			}
			if response.Code != tc.status || strings.Contains(response.Body.String(), "private database") {
				t.Fatalf("authentication failure misclassified: %d %s", response.Code, response.Body)
			}
			if tc.status == http.StatusServiceUnavailable {
				if response.Header().Get("Retry-After") != "1" || response.Header().Get("WWW-Authenticate") != "" {
					t.Fatalf("temporary failure claimed invalid token: %v", response.Header())
				}
			} else if response.Header().Get("WWW-Authenticate") == "" || response.Header().Get("Retry-After") != "" {
				t.Fatalf("invalid token did not challenge: %v", response.Header())
			}
		})
	}
}

func TestAuthenticationWriterWaitIsBoundedAndNotAnInvalidToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server, backend := testServer(t)
		server.auth = streamAuthFunc(func(ctx context.Context, _ string) (Actor, error) { <-ctx.Done(); return Actor{}, ctx.Err() })
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_get","arguments":{"id":"aaaaaaaa"}}}`))
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		started := time.Now()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable || time.Since(started) != 5*time.Second || backend.calledName != "" || response.Header().Get("WWW-Authenticate") != "" || response.Header().Get("Retry-After") != "1" {
			t.Fatalf("authentication timeout leaked into tool/invalid-token response: %d %s %s", response.Code, response.Body, time.Since(started))
		}
	})
}

func TestLogByteCursorValidationAndForwarding(t *testing.T) {
	for _, cursor := range []string{"-1", "bad", "9223372036854775808", "1&byte_offset=2"} {
		server, backend := testServer(t)
		request := httptest.NewRequest(http.MethodGet, "/api/runs/aaaaaaaa/logs?burn=t&step=u&byte_offset="+cursor, nil)
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || backend.logCalls != 0 {
			t.Fatalf("invalid cursor accepted: %s status=%d", cursor, response.Code)
		}
	}
	server, backend := testServer(t)
	request := httptest.NewRequest(http.MethodGet, "/api/runs/aaaaaaaa/logs?burn=t&step=u&byte_offset=4096&tail=true", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || backend.logFilter.ByteOffset == nil || *backend.logFilter.ByteOffset != 4096 || !backend.logFilter.Tail {
		t.Fatalf("byte cursor not forwarded: %d %+v", response.Code, backend.logFilter)
	}
}

func TestArtifactDownloadRejectsLogByteCursor(t *testing.T) {
	server, _ := testServer(t)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, artifactRequest(t, "/api/runs/aaaaaaaa/artifacts/report.txt?byte_offset=0"))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "only supported for run logs") {
		t.Fatalf("artifact ignored log cursor: %d %s", response.Code, response.Body)
	}
}
