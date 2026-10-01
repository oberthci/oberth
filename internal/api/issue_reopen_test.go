package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestIssueReopenMCPSchema(t *testing.T) {
	for _, definition := range toolDefinitions() {
		if definition["name"] != "issue_reopen" {
			continue
		}
		schema := definition["inputSchema"].(map[string]any)
		if !reflect.DeepEqual(schema["required"], []string{"id"}) || schema["additionalProperties"] != false {
			t.Fatalf("reopen schema = %#v", schema)
		}
		properties := schema["properties"].(map[string]any)
		if len(properties) != 1 || properties["id"].(map[string]any)["type"] != "integer" {
			t.Fatalf("reopen properties = %#v", properties)
		}
		return
	}
	t.Fatal("issue_reopen missing from MCP catalog")
}

func TestIssueReopenMCPRequiresAuthentication(t *testing.T) {
	for _, token := range []string{"", "invalid-token", "valid-token"} {
		t.Run(token, func(t *testing.T) {
			server, backend := testServer(t)
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"issue_reopen","arguments":{"id":278}}}`
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			if token != "" {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if token != "valid-token" {
				if response.Code != http.StatusUnauthorized || backend.calledName != "" {
					t.Fatalf("anonymous reopen reached backend: status %d, name %q", response.Code, backend.calledName)
				}
				return
			}
			if response.Code != http.StatusOK || backend.calledName != "issue_reopen" || backend.calledActor.Identity != "agent@host" || string(backend.calledArguments) != `{"id":278}` {
				t.Fatalf("authenticated reopen not bound to actor and ID: status %d, backend %#v", response.Code, backend)
			}
		})
	}
}
