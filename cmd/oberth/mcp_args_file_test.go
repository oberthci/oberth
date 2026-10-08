package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newMCPEchoServer returns a TLS test server that echoes tool arguments back
// as the text content, allowing tests to verify what the client sent.
func newMCPEchoServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			testWriteJSONRPCError(w, nil, -32700, "parse error")
			return
		}
		id := req.ID
		if req.Method == mcpMethodToolsCall {
			raw, _ := json.Marshal(req.Params)
			var params toolsCallParams
			_ = json.Unmarshal(raw, &params)
			// Echo back the arguments as text
			argsText := "{}"
			if params.Arguments != nil {
				argsText = string(params.Arguments)
			}
			testWriteJSONRPCResult(w, &id, toolsCallResult{
				Content: []mcpContent{{Type: "text", Text: fmt.Sprintf("tool=%s args=%s", params.Name, argsText)}},
			})
		} else {
			testWriteJSONRPCError(w, &id, -32601, "method not found")
		}
	}))
}

func TestMCPCallArgsFile(t *testing.T) {
	token := "test-args-file"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()

	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)

	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.json")
	argsContent := `{"id":42,"title":"test","body":"large body content"}`
	if err := os.WriteFile(argsPath, []byte(argsContent), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	err := runMCPCall(context.Background(), []string{"issue_update", "--args-file", argsPath}, &buf)
	if err != nil {
		t.Fatalf("runMCPCall with --args-file: %v", err)
	}
	if !strings.Contains(buf.String(), "tool=issue_update") {
		t.Fatalf("expected tool=issue_update in output, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"id":42`) {
		t.Fatalf("expected args echoed, got: %s", buf.String())
	}
}

func TestMCPCallArgsFileEqualsForm(t *testing.T) {
	token := "test-args-file-eq"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()

	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)

	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.json")
	if err := os.WriteFile(argsPath, []byte(`{"ref":"main"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	err := runMCPCall(context.Background(), []string{"status", "--args-file=" + argsPath}, &buf)
	if err != nil {
		t.Fatalf("runMCPCall with --args-file=: %v", err)
	}
	if !strings.Contains(buf.String(), `"ref":"main"`) {
		t.Fatalf("expected args echoed, got: %s", buf.String())
	}
}

func TestMCPCallArgsFileMutuallyExclusiveWithPositional(t *testing.T) {
	token := "test-mutual-excl"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()

	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)

	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.json")
	if err := os.WriteFile(argsPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	err := runMCPCall(context.Background(), []string{"status", `{"ref":"main"}`, "--args-file", argsPath}, &buf)
	if err == nil {
		t.Fatal("expected error for conflicting args-file and positional")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got: %v", err)
	}
}

func TestMCPCallArgsFileRejectsInvalidJSON(t *testing.T) {
	token := "test-invalid-json"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()

	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)

	dir := t.TempDir()
	argsPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(argsPath, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	err := runMCPCall(context.Background(), []string{"status", "--args-file", argsPath}, &buf)
	if err == nil {
		t.Fatal("expected error for invalid JSON file")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("expected invalid JSON error, got: %v", err)
	}
}

func TestMCPCallArgsFileMissing(t *testing.T) {
	t.Setenv("OBERTH_TOKEN", "tok")
	t.Setenv("OBERTH_MCP_URL", "https://example.invalid/mcp")

	var buf strings.Builder
	err := runMCPCall(context.Background(), []string{"status", "--args-file", "/nonexistent/path.json"}, &buf)
	if err == nil {
		t.Fatal("expected error for missing args file")
	}
	if !strings.Contains(err.Error(), "read args file") {
		t.Fatalf("expected read error, got: %v", err)
	}
}
