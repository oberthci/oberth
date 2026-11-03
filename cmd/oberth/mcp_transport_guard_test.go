package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestMCPRefusesCleartextBeforeCredentialsOrTransport(t *testing.T) {
	old := mcpHTTPClientFunc
	mcpHTTPClientFunc = func(string) (*http.Client, error) {
		t.Error("unsafe URL reached transport setup")
		return nil, errors.New("transport reached")
	}
	t.Cleanup(func() { mcpHTTPClientFunc = old })
	for _, address := range []string{"http://oberth.example/mcp", "http://10.0.0.1:8443/mcp", "http://[2001:db8::1]/mcp"} {
		_, err := mcpRequest(t.Context(), mcpFlagResult{mcpConfig: mcpConfig{URL: address, TokenCommand: "exit 1"}}, mcpMethodToolsList, nil)
		if err == nil || !strings.Contains(err.Error(), "cleartext") {
			t.Errorf("%s: %v", address, err)
		}
	}
}

func TestMCPLegacyDefaultWarns(t *testing.T) {
	t.Setenv("OBERTH_MCP_URL", "")
	t.Setenv("OBERTH_BASE_URL", "")
	t.Setenv("OBERTH_TOKEN", "fixture")
	oldStderr := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writer
	t.Cleanup(func() { os.Stderr = oldStderr; _ = reader.Close(); _ = writer.Close() })
	cfg, err := parseMCPFlags("test", nil, 0)
	if err != nil || cfg.URL != mcpLegacyDefaultURL {
		t.Fatalf("legacy default = %q: %v", cfg.URL, err)
	}
	_ = writer.Close()
	os.Stderr = oldStderr
	warning, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"deprecated", "--url", "OBERTH_MCP_URL", "OBERTH_BASE_URL"} {
		if !strings.Contains(string(warning), word) {
			t.Errorf("missing %q in warning %q", word, warning)
		}
	}
}
