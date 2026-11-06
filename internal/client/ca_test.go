package client

import (
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCAAcceptsPEMPreamblesAndBOM(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer server.Close()
	certificate := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	for _, prefix := range []string{"", "Bag Attributes\n    friendlyName: fixture\nsubject=CN=localhost\n", "\ufeff", "\ufeffsubject=CN=localhost\n"} {
		t.Run(prefix, func(t *testing.T) {
			c, err := New(t.Context(), Config{BaseURL: server.URL, Token: "fixture-token", CACert: prefix + certificate})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.GetRaw(t.Context(), "/api/status", nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCAErrorsNeverEchoInput(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "sensitive-value-not-a-file")
	_, err := LoadCAPool(missing)
	if !errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), missing) || strings.Contains(err.Error(), "sensitive-value") {
		t.Fatalf("unsafe read error: %v", err)
	}
	_, err = LoadCAPool("Bag Attributes\nsubject=sensitive-value\n-----BEGIN CERTIFICATE-----\nprivate-fixture-material\n-----END CERTIFICATE-----")
	if err == nil || strings.Contains(err.Error(), "sensitive-value") || strings.Contains(err.Error(), "private-fixture-material") {
		t.Fatalf("unsafe PEM error: %v", err)
	}
}
