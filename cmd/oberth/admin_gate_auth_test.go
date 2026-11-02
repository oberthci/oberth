package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestAdminGateRejectsMissingOrUnknownOperation(t *testing.T) {
	for _, operation := range []string{"", " ", "unknown.operation"} {
		called := false
		server := newAdminGateServer(func(context.Context) error { called = true; return nil }, "fixture", nil)
		request := httptest.NewRequest(http.MethodPost, "/v1/mutation-gate", nil)
		request.Header.Set("X-Oberth-Admin-Database", "fixture")
		if operation != "" {
			request.Header.Set("X-Oberth-Admin-Operation", operation)
		}
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || called {
			t.Fatalf("operation %q: status=%d gate=%v", operation, response.Code, called)
		}
	}
}

func TestDefaultAdminSocketUsesPrivateRunDirectory(t *testing.T) {
	if filepath.Dir(defaultAdminSocket) != "/run/oberth" {
		t.Fatalf("admin socket directory: %s", filepath.Dir(defaultAdminSocket))
	}
}

func TestAdminGateRecognizesBootstrapStorageOperations(t *testing.T) {
	for _, operation := range []string{"upstream.identity.oberth-upstream-key", "upstream.secret.oberth-known-hosts", "uplink.token.create", "run.cancel"} {
		if !validAdminOperation(operation) {
			t.Fatalf("supported operation rejected: %s", operation)
		}
	}
}

func privateAdminSocket(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "ob-admin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(directory) })
	return filepath.Join(directory, "admin.sock")
}
