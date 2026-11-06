package client

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientTransparentlyDecodesGzip(t *testing.T) {
	want := strings.Repeat("repository", 300)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "gzip" {
			t.Error("transport did not request gzip")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		if err := json.NewEncoder(writer).Encode(map[string]string{"name": want}); err != nil {
			t.Error(err)
		}
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c, err := New(t.Context(), Config{BaseURL: server.URL, Token: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if c.http.Transport.(*http.Transport).DisableCompression {
		t.Fatal("automatic gzip decoding disabled")
	}
	var got map[string]string
	if err := c.Get(t.Context(), "/api/repos", nil, &got); err != nil {
		t.Fatal(err)
	}
	if got["name"] != want {
		t.Fatal("JSON was not decoded")
	}
}
