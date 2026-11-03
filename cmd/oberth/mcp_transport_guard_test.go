package main

import (
	"errors"
	"io"
	"net/http"
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

func TestMCPRequiresConfiguredEndpointBeforeCredentialsOrTransport(t *testing.T) {
	t.Setenv("OBERTH_MCP_URL", "")
	t.Setenv("OBERTH_BASE_URL", "")
	t.Setenv("OBERTH_TOKEN", "fixture")
	t.Setenv("OBERTH_TOKEN_COMMAND", "exit 99")
	old := mcpHTTPClientFunc
	mcpHTTPClientFunc = func(string) (*http.Client, error) {
		t.Error("missing endpoint reached transport setup")
		return nil, errors.New("transport reached")
	}
	t.Cleanup(func() { mcpHTTPClientFunc = old })
	for _, arguments := range [][]string{nil, {"--token-file", "/missing-token-file"}} {
		err := runMCPTools(t.Context(), arguments, io.Discard)
		for _, option := range []string{"--url", "OBERTH_MCP_URL", "OBERTH_BASE_URL"} {
			if err == nil || !strings.Contains(err.Error(), option) {
				t.Errorf("missing endpoint must name %s before reading a token: %v", option, err)
			}
		}
	}
}
