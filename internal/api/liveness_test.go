package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLivenessIsPublicIndependentOfReadinessAndDrainSafe(t *testing.T) {
	backend := &fakeBackend{readyErr: errors.New("external dependencies unavailable")}
	var liveErr error
	calls := 0
	server, err := New(backend, backend, backend, "test", WithLiveness(func() error { calls++; return liveErr }))
	if err != nil {
		t.Fatal(err)
	}
	check := func(path string, want int) {
		t.Helper()
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != want {
			t.Fatalf("%s = %d %s, want %d", path, response.Code, response.Body.String(), want)
		}
		if strings.Contains(response.Body.String(), "sensitive") {
			t.Fatal("probe exposed error details")
		}
	}
	check("/livez", http.StatusOK)
	check("/readyz", http.StatusServiceUnavailable)
	liveErr = errors.New("sensitive internal details")
	check("/livez", http.StatusServiceUnavailable)
	check("/healthz", http.StatusOK)
	if calls != 2 {
		t.Fatalf("liveness calls = %d", calls)
	}
	server.BeginDrain()
	check("/livez", http.StatusOK)
	check("/healthz", http.StatusOK)
	check("/readyz", http.StatusServiceUnavailable)
	if calls != 2 {
		t.Fatal("draining server consulted stopped workers")
	}
}

func TestLivenessWithoutProgressCheckFailsClosed(t *testing.T) {
	backend := &fakeBackend{}
	server, err := New(backend, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatal("unconfigured liveness passed")
	}
}
