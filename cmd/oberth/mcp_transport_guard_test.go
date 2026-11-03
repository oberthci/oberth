package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMCPArtifactOutputRefusesCleartextBeforeCredentialsOrTransport(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected artifact request", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	// Route an unsafe test hostname only to the local listener. The vulnerable
	// path must be observable without sending even a fixture token off-host.
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}
	t.Cleanup(transport.CloseIdleConnections)
	old := mcpHTTPClientFunc
	transports := 0
	mcpHTTPClientFunc = func(string) (*http.Client, error) {
		transports++
		return &http.Client{Transport: transport}, nil
	}
	t.Cleanup(func() { mcpHTTPClientFunc = old })
	for _, source := range []string{"flag", "MCP environment", "base environment"} {
		for _, credential := range []string{"environment", "command", "file"} {
			t.Run(source+"/"+credential, func(t *testing.T) {
				t.Setenv("OBERTH_MCP_URL", "")
				t.Setenv("OBERTH_BASE_URL", "")
				t.Setenv("OBERTH_TOKEN", "")
				t.Setenv("OBERTH_TOKEN_COMMAND", "")
				output := filepath.Join(t.TempDir(), "artifact.bin")
				marker := filepath.Join(t.TempDir(), "token-command-ran")
				t.Setenv("OBERTH_MCP_TEST_TOKEN_MARKER", marker)
				args := []string{"artifact_get", `{"id":"run1","name":"a.txt"}`, "--output", output}
				switch source {
				case "flag":
					args = append(args, "--url", "http://oberth.example/mcp")
				case "MCP environment":
					t.Setenv("OBERTH_MCP_URL", "http://oberth.example/mcp")
				case "base environment":
					t.Setenv("OBERTH_BASE_URL", "http://oberth.example")
				}
				switch credential {
				case "environment":
					t.Setenv("OBERTH_TOKEN", "fixture")
				case "command":
					t.Setenv("OBERTH_TOKEN_COMMAND", `printf invoked > "$OBERTH_MCP_TEST_TOKEN_MARKER"; printf fixture`)
				case "file":
					args = append(args, "--token-file", filepath.Join(t.TempDir(), "missing-token"))
				}
				err := runMCPCall(t.Context(), args, io.Discard)
				if err == nil || !strings.Contains(err.Error(), "cleartext") {
					t.Errorf("unsafe artifact URL must be refused before credentials: %v", err)
				}
				for _, path := range []string{marker, output} {
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("unsafe URL created %s: %v", filepath.Base(path), err)
					}
				}
			})
		}
	}
	if transports != 0 || requests.Load() != 0 {
		t.Errorf("unsafe artifact URL reached transport: clients=%d requests=%d", transports, requests.Load())
	}
}

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
