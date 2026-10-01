package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newMCPTestServer returns an httptest TLS server that speaks a minimal MCP
// Streamable HTTP JSON-RPC subset: tools/list and tools/call.
func newMCPTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("MCP-Protocol-Version"); got != mcpProtocolVersion {
			http.Error(w, "bad MCP-Protocol-Version", http.StatusBadRequest)
			return
		}
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "oberth-cli/") {
			http.Error(w, "bad User-Agent", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			testWriteJSONRPCError(w, nil, -32700, "parse error")
			return
		}
		id := req.ID

		switch req.Method {
		case mcpMethodToolsList:
			testWriteJSONRPCResult(w, &id, toolsListResult{
				Tools: []mcpTool{
					{Name: "system_status", Description: "Show system status"},
					{Name: "status", Description: "CI status for a ref"},
				},
			})
		case mcpMethodToolsCall:
			raw, _ := json.Marshal(req.Params)
			var params toolsCallParams
			if err := json.Unmarshal(raw, &params); err != nil {
				testWriteJSONRPCError(w, &id, -32602, "invalid params")
				return
			}
			switch params.Name {
			case "system_status":
				testWriteJSONRPCResult(w, &id, toolsCallResult{
					Content: []mcpContent{{Type: "text", Text: `{"status":"ready"}`}},
				})
			case "error_tool":
				testWriteJSONRPCResult(w, &id, toolsCallResult{
					Content: []mcpContent{{Type: "text", Text: "something went wrong"}},
					IsError: true,
				})
			default:
				testWriteJSONRPCError(w, &id, -32601, fmt.Sprintf("tool not found: %s", params.Name))
			}
		default:
			testWriteJSONRPCError(w, &id, -32601, "method not found")
		}
	}))
}

// newMCPSSETestServer returns an httptest TLS server that responds with
// SSE-framed JSON-RPC responses.
func newMCPSSETestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		var req jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		result := toolsListResult{
			Tools: []mcpTool{{Name: "sse_tool", Description: "SSE-delivered tool"}},
		}
		resultBytes, _ := json.Marshal(result)
		response := jsonRPCResponse{
			JSONRPC: mcpJSONRPCVersion,
			ID:      &req.ID,
			Result:  resultBytes,
		}
		responseBytes, _ := json.Marshal(response)

		w.Header().Set("Content-Type", mcpContentTypeSSE)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", responseBytes)
	}))
}

func testWriteJSONRPCResult(w http.ResponseWriter, id *int, result any) {
	resultBytes, _ := json.Marshal(result)
	w.Header().Set("Content-Type", mcpContentTypeJSON)
	_ = json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: mcpJSONRPCVersion,
		ID:      id,
		Result:  resultBytes,
	})
}

func testWriteJSONRPCError(w http.ResponseWriter, id *int, code int, message string) {
	w.Header().Set("Content-Type", mcpContentTypeJSON)
	_ = json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: mcpJSONRPCVersion,
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: message},
	})
}

func withTestMCPClient(t *testing.T, server *httptest.Server) func() {
	t.Helper()
	orig := mcpHTTPClientFunc
	mcpHTTPClientFunc = func(_ string) (*http.Client, error) {
		return server.Client(), nil
	}
	return func() { mcpHTTPClientFunc = orig }
}

func TestMCPToolsList(t *testing.T) {
	token := "test-token-tools-list"
	server := newMCPTestServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()

	cfg := mcpFlagResult{mcpConfig: mcpConfig{URL: server.URL, Token: token, Version: "test"}}
	response, err := mcpRequest(context.Background(), cfg, mcpMethodToolsList, nil)
	if err != nil {
		t.Fatalf("mcpRequest: %v", err)
	}
	if response.Error != nil {
		t.Fatalf("unexpected error: %s", response.Error.Message)
	}
	var result toolsListResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(result.Tools))
	}
	if result.Tools[0].Name != "system_status" {
		t.Errorf("expected first tool system_status, got %s", result.Tools[0].Name)
	}
}

func TestMCPCallSuccess(t *testing.T) {
	token := "test-token-call"
	server := newMCPTestServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()

	cfg := mcpFlagResult{mcpConfig: mcpConfig{URL: server.URL, Token: token, Version: "test"}}
	params := toolsCallParams{Name: "system_status"}
	response, err := mcpRequest(context.Background(), cfg, mcpMethodToolsCall, params)
	if err != nil {
		t.Fatalf("mcpRequest: %v", err)
	}
	if response.Error != nil {
		t.Fatalf("unexpected error: %s", response.Error.Message)
	}
	var result toolsCallResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.IsError {
		t.Error("expected isError=false")
	}
	if len(result.Content) != 1 || result.Content[0].Text != `{"status":"ready"}` {
		t.Errorf("unexpected content: %+v", result.Content)
	}
}

func TestMCPCallIsError(t *testing.T) {
	token := "test-token-iserror"
	server := newMCPTestServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()

	cfg := mcpFlagResult{mcpConfig: mcpConfig{URL: server.URL, Token: token, Version: "test"}}
	params := toolsCallParams{Name: "error_tool"}
	response, err := mcpRequest(context.Background(), cfg, mcpMethodToolsCall, params)
	if err != nil {
		t.Fatalf("mcpRequest: %v", err)
	}
	var result toolsCallResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.IsError {
		t.Error("expected isError=true")
	}
	if len(result.Content) == 0 || result.Content[0].Text != "something went wrong" {
		t.Errorf("unexpected content: %+v", result.Content)
	}
}

func TestMCPUnauthorized(t *testing.T) {
	server := newMCPTestServer(t, "correct-token")
	defer server.Close()
	defer withTestMCPClient(t, server)()

	cfg := mcpFlagResult{mcpConfig: mcpConfig{URL: server.URL, Token: "wrong-token", Version: "test"}}
	_, err := mcpRequest(context.Background(), cfg, mcpMethodToolsList, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "authentication failed") {
		t.Errorf("expected authentication error, got: %v", err)
	}
}

func TestMCPSSEResponse(t *testing.T) {
	token := "test-token-sse"
	server := newMCPSSETestServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()

	cfg := mcpFlagResult{mcpConfig: mcpConfig{URL: server.URL, Token: token, Version: "test"}}
	response, err := mcpRequest(context.Background(), cfg, mcpMethodToolsList, nil)
	if err != nil {
		t.Fatalf("mcpRequest SSE: %v", err)
	}
	if response.Error != nil {
		t.Fatalf("unexpected error: %s", response.Error.Message)
	}
	var result toolsListResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Tools) != 1 || result.Tools[0].Name != "sse_tool" {
		t.Errorf("unexpected SSE tools result: %+v", result.Tools)
	}
}

func TestMCPTokenFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("test-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OBERTH_MCP_URL", "https://example.invalid/mcp")
	t.Setenv("OBERTH_TOKEN", "")

	_, err := parseMCPFlags("test", []string{"--token-file", path}, 0)
	if err == nil {
		t.Fatal("expected permission error for 0644 token file")
	}
	if !strings.Contains(err.Error(), "0644") {
		t.Errorf("expected mode in error message, got: %v", err)
	}
}

func TestMCPTokenFileValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("valid-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OBERTH_MCP_URL", "https://example.invalid/mcp")
	t.Setenv("OBERTH_TOKEN", "")

	result, err := parseMCPFlags("test", []string{"--token-file", path}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Token != "valid-token" {
		t.Errorf("expected token 'valid-token', got %q", result.Token)
	}
}

func TestMCPParseFlagsURL(t *testing.T) {
	t.Setenv("OBERTH_TOKEN", "env-token")
	t.Setenv("OBERTH_MCP_URL", "")

	result, err := parseMCPFlags("test", []string{"--url", "https://custom.example/mcp"}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.URL != "https://custom.example/mcp" {
		t.Errorf("expected custom URL, got %q", result.URL)
	}
}

func TestMCPParseFlagsDefault(t *testing.T) {
	t.Setenv("OBERTH_TOKEN", "env-token")
	t.Setenv("OBERTH_MCP_URL", "")

	result, err := parseMCPFlags("test", nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.URL != mcpDefaultURL {
		t.Errorf("expected default URL %q, got %q", mcpDefaultURL, result.URL)
	}
}

func TestMCPRunCLIHelp(t *testing.T) {
	var buf strings.Builder
	err := runCLI(context.Background(), []string{"mcp", "--help"}, nil, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "oberth mcp") {
		t.Errorf("expected help text, got: %s", buf.String())
	}
}

func TestMCPRunCLIUsage(t *testing.T) {
	var buf strings.Builder
	err := runCLI(context.Background(), []string{"mcp"}, nil, &buf)
	if err == nil {
		t.Fatal("expected usage error")
	}
}

func TestMCPCallEndToEnd(t *testing.T) {
	token := "test-e2e-token"
	server := newMCPTestServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()

	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)

	var buf strings.Builder
	err := runMCPCall(context.Background(), []string{"system_status"}, &buf)
	if err != nil {
		t.Fatalf("runMCPCall: %v", err)
	}
	if !strings.Contains(buf.String(), `"status":"ready"`) {
		t.Errorf("expected status ready in output, got: %s", buf.String())
	}
}

func TestMCPCallEndToEndIsError(t *testing.T) {
	token := "test-e2e-iserror"
	server := newMCPTestServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()

	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)

	var buf strings.Builder
	err := runMCPCall(context.Background(), []string{"error_tool"}, &buf)
	if err == nil {
		t.Fatal("expected error for isError tool")
	}
	if !errors.Is(err, errMCPToolError) {
		t.Errorf("expected errMCPToolError, got: %v", err)
	}
}

func TestMCPToolsEndToEnd(t *testing.T) {
	token := "test-e2e-tools"
	server := newMCPTestServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()

	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)

	var buf strings.Builder
	err := runMCPTools(context.Background(), nil, &buf)
	if err != nil {
		t.Fatalf("runMCPTools: %v", err)
	}
	output := buf.String()
	if !strings.Contains(output, "system_status") {
		t.Errorf("expected system_status in output, got: %s", output)
	}
	if !strings.Contains(output, "status") {
		t.Errorf("expected status in output, got: %s", output)
	}
}
