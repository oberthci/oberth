package main

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func (f *serverFixture) healthyStore(t *testing.T) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/sys/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	f.config.SecretStore.Address = server.URL
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(f.config.Files.SecretStoreCA, ca, 0600); err != nil {
		t.Fatal(err)
	}
}
