package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInitializeNegotiatesSupportedOrFallbackVersion(t *testing.T) {
	for _, cell := range []struct{ requested, want string }{
		{"2025-03-26", "2025-03-26"},
		{"2025-06-18", "2025-06-18"},
		{"2024-11-05", "2025-06-18"},
		{"2099-01-01", "2025-06-18"},
	} {
		t.Run(cell.requested, func(t *testing.T) {
			s, _ := testServer(t)
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"test","version":"1"},"_meta":{"test":true}}}`, cell.requested)))
			req.Header.Set("Authorization", "Bearer valid-token")
			out := httptest.NewRecorder()
			s.Handler().ServeHTTP(out, req)
			var response struct {
				Error  *rpcError
				Result struct {
					Version string `json:"protocolVersion"`
				}
			}
			if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if out.Code != http.StatusOK || response.Error != nil || response.Result.Version != cell.want {
				t.Fatalf("status=%d response=%s, want version %q", out.Code, out.Body, cell.want)
			}
		})
	}
}

func negotiationPost(t *testing.T, s *Server, body, version, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, req)
	return out
}

func TestInitializeRejectsInvalidProtocolVersion(t *testing.T) {
	for _, params := range []string{
		``, `null`, `[]`, `42`, `"version"`, `{}`,
		`{"protocolVersion":null}`, `{"protocolVersion":true}`,
		`{"protocolVersion":20250326}`, `{"protocolVersion":[]}`,
		`{"protocolVersion":{}}`, `{"protocolVersion":""}`,
		`{"protocolVersion":" 2025-03-26"}`, `{"protocolVersion":"2025-03-26 "}`,
		`{"protocolVersion":"latest"}`, `{"protocolVersion":"2025-02-30"}`,
	} {
		t.Run(params, func(t *testing.T) {
			s, backend := testServer(t)
			body := `{"jsonrpc":"2.0","id":7,"method":"initialize"`
			if params != "" {
				body += `,"params":` + params
			}
			out := negotiationPost(t, s, body+`}`, "", "valid-token")
			var response rpcResponse
			if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if out.Code != http.StatusOK || response.Error == nil || response.Error.Code != -32602 || string(response.ID) != "7" || response.Result != nil || backend.calledName != "" {
				t.Fatalf("invalid initialize: status=%d response=%s backend=%q", out.Code, out.Body, backend.calledName)
			}
		})
	}
}

func TestMCPStatelessInitializationSequence(t *testing.T) {
	s, backend := testServer(t)
	// Direct authenticated calls remain supported: there is no connection or
	// token-wide initialization state, including across failed/repeated init.
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":4,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":5,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	} {
		out := negotiationPost(t, s, body, "", "valid-token")
		if strings.Contains(body, `"method":"notifications/initialized"`) {
			if out.Code != http.StatusAccepted || out.Body.Len() != 0 {
				t.Fatalf("notification: status=%d body=%s", out.Code, out.Body)
			}
		} else {
			var response rpcResponse
			if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			invalid := strings.Contains(body, `"params":{}`)
			if out.Code != http.StatusOK || (response.Error != nil) != invalid {
				t.Fatalf("sequence: status=%d body=%s", out.Code, out.Body)
			}
		}
		if out.Header().Get("Mcp-Session-Id") != "" || backend.calledName != "" {
			t.Fatalf("handshake created session or invoked backend: %v %q", out.Header(), backend.calledName)
		}
	}
	for _, version := range []string{"", "2025-03-26", "2025-06-18"} {
		backend.calledName = ""
		backend.calledActor = Actor{}
		out := negotiationPost(t, s, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"status","arguments":{}}}`, version, "valid-token")
		var response rpcResponse
		if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if out.Code != http.StatusOK || response.Error != nil || response.Result == nil || backend.calledName != "status" || backend.calledActor.Identity == "" {
			t.Fatalf("call after initialization: status=%d body=%s backend=%q", out.Code, out.Body, backend.calledName)
		}
	}
}

func TestMCPRejectsInvalidHandshakeDirectionAndVersionHeader(t *testing.T) {
	for _, cell := range []struct {
		body, version string
		status        int
	}{
		{`{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":"2025-03-26"}}`, "", http.StatusBadRequest},
		{`{"jsonrpc":"2.0","id":1,"method":"notifications/initialized"}`, "", http.StatusOK},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status"}}`, "2099-01-01", http.StatusBadRequest},
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, "latest", http.StatusBadRequest},
		{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2099-01-01"}}`, "2099-01-01", http.StatusBadRequest},
	} {
		s, backend := testServer(t)
		out := negotiationPost(t, s, cell.body, cell.version, "valid-token")
		if out.Code != cell.status || backend.calledName != "" {
			t.Fatalf("invalid handshake/header: status=%d body=%s backend=%q", out.Code, out.Body, backend.calledName)
		}
		if cell.status == http.StatusOK {
			var response rpcResponse
			if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil || response.Error == nil || response.Error.Code != -32600 {
				t.Fatalf("notification sent as request: %s (%v)", out.Body, err)
			}
		}
	}
}

func TestMCPNegotiationPreservesAuthAndCapabilities(t *testing.T) {
	s, backend := testServer(t)
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	} {
		if out := negotiationPost(t, s, body, "", ""); out.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated handshake: status=%d body=%s", out.Code, out.Body)
		}
	}
	out := negotiationPost(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, "2025-03-26", "valid-token")
	var response struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if out.Code != http.StatusOK || response.Result.ProtocolVersion != "2025-06-18" || len(response.Result.Capabilities) != 1 || response.Result.Capabilities["tools"] == nil {
		t.Fatalf("initialize capabilities: status=%d body=%s", out.Code, out.Body)
	}
	for _, method := range []string{"resources/list", "prompts/list"} {
		out := negotiationPost(t, s, fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":%q}`, method), "2025-06-18", "valid-token")
		var response rpcResponse
		if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil || response.Error == nil || response.Error.Code != -32601 {
			t.Fatalf("unadvertised method: %s (%v)", out.Body, err)
		}
	}
	if backend.calledName != "" {
		t.Fatalf("negotiation invoked %q", backend.calledName)
	}
}

func TestMCPVersionHeaderCannotHideAnUnsupportedValue(t *testing.T) {
	for _, versions := range [][]string{
		{""}, {"2025-03-26, 2025-06-18"}, {" 2025-06-18"},
		{"2025-06-18", "2099-01-01"},
	} {
		s, backend := testServer(t)
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status"}}`))
		req.Header.Set("Authorization", "Bearer valid-token")
		for _, version := range versions {
			req.Header.Add("MCP-Protocol-Version", version)
		}
		out := httptest.NewRecorder()
		s.Handler().ServeHTTP(out, req)
		if out.Code != http.StatusBadRequest || backend.calledName != "" {
			t.Fatalf("headers=%v status=%d body=%s backend=%q", versions, out.Code, out.Body, backend.calledName)
		}
	}
}
