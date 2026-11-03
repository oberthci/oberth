package main

import (
	"encoding/pem"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPInstallerEnvironment(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/mcp" || r.Header.Get("Authorization") != "Bearer command-token" {
			t.Errorf("request path=%q auth matches=%v", r.URL.Path, r.Header.Get("Authorization") == "Bearer command-token")
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		id := 1
		testWriteJSONRPCResult(w, &id, toolsListResult{Tools: []mcpTool{{Name: "system_status"}}})
	}))
	t.Cleanup(server.Close)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OBERTH_MCP_URL", "")
	t.Setenv("OBERTH_BASE_URL", server.URL+"/prefix/")
	t.Setenv("OBERTH_TOKEN", "")
	t.Setenv("OBERTH_TOKEN_COMMAND", "printf command-token")
	t.Setenv("OBERTH_CA_CERT", caFile)
	var out strings.Builder
	if err := runMCPTools(t.Context(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "system_status") {
		t.Fatalf("response = %q", out.String())
	}
}

func TestMCPEnvironmentPrecedence(t *testing.T) {
	t.Setenv("OBERTH_BASE_URL", "https://base.example/prefix/")
	t.Setenv("OBERTH_MCP_URL", "https://mcp.example/endpoint")
	t.Setenv("OBERTH_TOKEN", "environment-token")
	t.Setenv("OBERTH_TOKEN_COMMAND", "exit 1")
	t.Setenv("OBERTH_CA_CERT", "/environment/ca.pem")
	for _, test := range []struct {
		name    string
		args    []string
		url, ca string
	}{
		{"environment", nil, "https://mcp.example/endpoint", "/environment/ca.pem"},
		{"flags", []string{"--url=https://flag.example/mcp", "--ca-file=/flag/ca.pem"}, "https://flag.example/mcp", "/flag/ca.pem"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := parseMCPFlags("test", test.args, 0)
			if err != nil || cfg.URL != test.url || cfg.CAFile != test.ca || cfg.Token != "environment-token" {
				t.Fatalf("url=%q ca=%q err=%v", cfg.URL, cfg.CAFile, err)
			}
		})
	}
}

func TestProductCodeHasNoHostedInstanceURL(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, dir := range []string{"cmd", "internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(body), "watch."+"oberth.ci") {
				t.Errorf("hosted instance URL in %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestMCPUnconfiguredEndpointDoesNotCreateClient(t *testing.T) {
	t.Setenv("OBERTH_BASE_URL", "")
	t.Setenv("OBERTH_MCP_URL", "")
	t.Setenv("OBERTH_TOKEN", "test-token")
	original := mcpHTTPClientFunc
	mcpHTTPClientFunc = func(string) (*http.Client, error) {
		t.Fatal("unconfigured command attempted network setup")
		return nil, nil
	}
	t.Cleanup(func() { mcpHTTPClientFunc = original })
	var out strings.Builder
	err := runMCPTools(t.Context(), nil, &out)
	if err == nil {
		t.Fatal("unconfigured endpoint accepted")
	}
	for _, option := range []string{"--url", "OBERTH_MCP_URL", "OBERTH_BASE_URL"} {
		if !strings.Contains(err.Error(), option) {
			t.Errorf("missing %s in %v", option, err)
		}
	}
}
