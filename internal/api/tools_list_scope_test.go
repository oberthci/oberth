package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type adminAuthenticator struct{}

func (adminAuthenticator) Authenticate(_ context.Context, token string) (Actor, error) {
	if token != "admin-token" {
		return Actor{}, ErrInvalidToken
	}
	return Actor{Identity: "operator@host", Admin: true}, nil
}

func listedTools(t *testing.T, server *Server, token string) map[string]bool {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var decoded struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &decoded) != nil {
		t.Fatalf("tools/list = %d %s", response.Code, response.Body)
	}
	names := map[string]bool{}
	for _, tool := range decoded.Result.Tools {
		names[tool.Name] = true
	}
	return names
}

// A non-admin token is not offered the tools it can only be refused; an
// admin token sees every registered tool, and a non-admin call to a hidden
// tool still reaches the service, which refuses it (#P2-5).
func TestToolsListHidesAdminOnlyToolsFromNonAdmins(t *testing.T) {
	_, backend := testServer(t)
	nonAdmin, err := New(backend, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := New(adminAuthenticator{}, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	all := ToolNames()
	var adminOnly []string
	for _, name := range all {
		if AdminOnlyTool(name) {
			adminOnly = append(adminOnly, name)
		}
	}
	if len(adminOnly) != len(adminOnlyTools) {
		t.Fatalf("adminOnlyTools names %d tools, %d of them registered", len(adminOnlyTools), len(adminOnly))
	}
	seen := listedTools(t, nonAdmin, "valid-token")
	if len(seen) != len(all)-len(adminOnly) {
		t.Fatalf("non-admin sees %d tools, want %d", len(seen), len(all)-len(adminOnly))
	}
	for _, name := range adminOnly {
		if seen[name] {
			t.Errorf("non-admin offered admin-only %s", name)
		}
	}
	for _, name := range []string{"run_wait", "events_wait", "run_list", "run_hold_exec", "run_cancel", "promote", "system_status"} {
		if !seen[name] {
			t.Errorf("non-admin not offered %s", name)
		}
	}
	if everything := listedTools(t, admin, "admin-token"); len(everything) != len(all) {
		t.Fatalf("admin sees %d tools, want %d", len(everything), len(all))
	}
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"uplink_list","arguments":{}}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	nonAdmin.Handler().ServeHTTP(httptest.NewRecorder(), request)
	if backend.calledName != "uplink_list" {
		t.Fatalf("hidden tool call not passed to the service: %q", backend.calledName)
	}
}
