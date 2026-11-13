package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/api"
)

func TestAdministrationClient(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/admin/version" || r.Header.Get("Authorization") != "Bearer admin" {
			t.Errorf("unexpected admin request: %s %s", r.Method, r.URL)
		}
		_, _ = io.WriteString(w, `{"version":"test","schema":1}`)
	}))
	defer server.Close()
	client, err := New(t.Context(), Config{BaseURL: server.URL, Token: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Version string `json:"version"`
	}
	if err := client.Admin(t.Context(), "version", struct{}{}, &result); err != nil || result.Version != "test" {
		t.Fatalf("admin call: %+v %v", result, err)
	}
	if err := client.Admin(t.Context(), "../private", struct{}{}, nil); err == nil {
		t.Fatal("accepted action traversal")
	}
	if err := client.Admin(t.Context(), "version", map[string]string{"value": strings.Repeat("x", api.AdminRequestBytes)}, nil); err == nil {
		t.Fatal("accepted oversized arguments")
	}
	if calls.Load() != 1 {
		t.Fatalf("invalid arguments reached server: calls=%d", calls.Load())
	}
}

func TestAdministrationClientNeverRedirectsOrRetries(t *testing.T) {
	var requests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1); t.Error("admin body followed a redirect") }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client, err := New(context.Background(), Config{BaseURL: source.URL, Token: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Admin(t.Context(), "upstream-provide-key", api.UpstreamKeyRequest{Name: "north", PrivateKey: "sensitive-input"}, nil); err == nil {
		t.Fatal("redirect accepted")
	}
	if requests.Load() != 1 {
		t.Fatalf("admin redirect/retry requests=%d", requests.Load())
	}
}

func TestAdministrationBackupClientBounds(t *testing.T) {
	var oversized atomic.Bool
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/admin/database-backup" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer admin" {
			t.Errorf("unexpected backup request: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if oversized.Load() {
			_, _ = io.WriteString(w, strings.Repeat("x", (64<<10)+2))
			return
		}
		_, _ = io.WriteString(w, "archive")
	}))
	defer server.Close()
	client, err := New(t.Context(), Config{BaseURL: server.URL + "/", Token: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := client.DownloadAdminBackup(t.Context(), 1, &output); err != nil || output.String() != "archive" {
		t.Fatalf("backup: %q %v", output.String(), err)
	}
	oversized.Store(true)
	if err := client.DownloadAdminBackup(t.Context(), 1, io.Discard); err == nil {
		t.Fatal("accepted oversized backup response")
	}
	for _, limit := range []int64{-1, 0, (4 << 30) + 1} {
		if err := client.DownloadAdminBackup(t.Context(), limit, io.Discard); err == nil {
			t.Fatalf("accepted invalid bound %d", limit)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("invalid bounds reached server: calls=%d", calls.Load())
	}
}
