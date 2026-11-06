package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPClientTransparentlyDecodesGzip(t *testing.T) {
	want := strings.Repeat("output ", 300)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "gzip" {
			t.Error("MCP transport did not request gzip")
		}
		var request jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"content": []any{map[string]string{"type": "text", "text": want}}}}
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			t.Error(err)
		}
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c, err := mcpHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Transport.(*http.Transport).DisableCompression {
		t.Fatal("automatic gzip decoding disabled")
	}
	t.Setenv("OBERTH_TOKEN", "fixture")
	t.Setenv("OBERTH_MCP_URL", server.URL+"/mcp")
	t.Setenv("OBERTH_TOKEN_COMMAND", "")
	var out bytes.Buffer
	if err := runMCPCall(t.Context(), []string{"run_logs", `{"id":"run-abc"}`}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), want) {
		t.Fatal("MCP response was not decoded", out.String())
	}
}
