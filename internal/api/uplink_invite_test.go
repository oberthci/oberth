package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type inviteTestBackend struct {
	fakeBackend
	calls int
}

func (b *inviteTestBackend) RedeemInvite(_ context.Context, raw json.RawMessage) (any, error) {
	b.calls++
	return nil, errors.New(string(raw))
}
func TestInviteMCPEndpointIsIsolatedAndRedactsFailures(t *testing.T) {
	backend := &inviteTestBackend{}
	server, err := New(backend, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	list := request("/mcp/enroll", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "uplink_redeem") || strings.Contains(list.Body.String(), "uplink_invite") {
		t.Fatal("enrollment exposed ordinary tools")
	}
	forbidden := request("/mcp/enroll", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"uplink_list","arguments":{}}}`)
	if !strings.Contains(forbidden.Body.String(), "only uplink_redeem") || backend.calls != 0 || backend.calledName != "" {
		t.Fatal("enrollment invoked an ordinary tool")
	}
	body := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"uplink_redeem","arguments":{"secret":"private-request-canary"}}}`
	refused := request("/mcp/enroll", body)
	if backend.calls != 1 || strings.Contains(refused.Body.String(), "private-request-canary") || refused.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("redemption error disclosed request data")
	}
	authenticated := request("/mcp", `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	if authenticated.Code != http.StatusUnauthorized {
		t.Fatal("enrollment opened the ordinary MCP endpoint")
	}
	oversized := request("/mcp/enroll", strings.Repeat(" ", 100<<10))
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Fatal("enrollment request was not bounded")
	}
}
