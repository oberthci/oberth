package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

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
