package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAdminRemoteCommands(t *testing.T) {
	for _, name := range []string{"OBERTH_MCP_URL", "OBERTH_BASE_URL", "OBERTH_TOKEN_COMMAND", "OBERTH_CA_CERT", "OBERTH_CLIENT_CERT", "OBERTH_CLIENT_KEY"} {
		t.Setenv(name, "")
	}
	t.Setenv("OBERTH_TOKEN", "admin-fixture")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing-kubeconfig"))
	var called atomic.Value
	called.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin-fixture" {
			t.Error("admin credential not forwarded")
		}
		called.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/mcp" {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				t.Error("invalid MCP request")
			}
			called.Store(request.Params.Name)
			if request.Params.Name == "audit_list" {
				payload := map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"isError": false, "content": []any{map[string]any{"type": "text", "text": `{"head_id":1,"entries":[{"id":1,"verified":true,"sha256":"` + strings.Repeat("a", 64) + `"}]}`}}}}
				_ = json.NewEncoder(w).Encode(payload)
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":{"isError":false,"content":[],"structuredContent":{"ok":true}}}`))
			return
		}
		switch r.URL.Path {
		case "/api/admin/upstream-list":
			_, _ = w.Write([]byte(`{"upstreams":[]}`))
		case "/api/admin/upstream-add":
			var request struct {
				Dedicated bool `json:"dedicated_key"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				t.Error("invalid upstream request")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"identity": map[string]any{"pending": true, "public_key": "public key", "restart_required": request.Dedicated}, "upstream": map[string]string{"name": "north"}})
		case "/api/admin/idle":
			_, _ = w.Write([]byte(`{"busy":false,"queue":{"version":1}}`))
		case "/api/admin/public-identity":
			_, _ = w.Write([]byte(`{"content":"public certificate"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer server.Close()
	t.Setenv("OBERTH_BASE_URL", server.URL)
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"upstream", "list", "--check", "--json"}, "/api/admin/upstream-list"},
		{[]string{"upstream", "add", "--yes", "--no-wait", "north", "ssh://source.invalid/team"}, "/api/admin/upstream-add"},
		{[]string{"upstream", "add", "--yes", "--dedicated-key", "north", "ssh://source.invalid/team"}, "/api/admin/upstream-add"},
		{[]string{"repo", "add", "repository", "north"}, "/api/admin/repo-add"},
		{[]string{"repo", "verify", "north/team/repository"}, "/api/admin/repo-verify"},
		{[]string{"admin", "public-identity", "server-ca"}, "/api/admin/public-identity"},
		{[]string{"wait-idle"}, "/api/admin/idle"},
		{[]string{"audit", "head"}, "audit_list"},
		{[]string{"audit", "record", "backup.completed"}, "/api/admin/audit-record"},
		{[]string{"admin", "readiness"}, "/api/admin/readiness"},
		{[]string{"uplink", "add", "-", "operator@host"}, "/api/admin/uplink-add"},
		{[]string{"uplink", "scope", "--clear", "operator@host"}, "/api/admin/uplink-scope"},
		{[]string{"access", "allow", "repository", ".oberth/build.yaml", "build", "oberth/data/path"}, "access_allow"},
		{[]string{"repo", "remove", "repository"}, "repo_remove"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			called.Store("")
			var output bytes.Buffer
			if err := runCLI(context.Background(), test.args, strings.NewReader(""), &output); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(" "+strings.Join(test.args, " ")+" ", " --dedicated-key ") && !strings.Contains(output.String(), "Restart the Oberth server Deployment") {
				t.Fatal("dedicated key response omitted activation instruction")
			}
			if called.Load() != test.want {
				t.Fatalf("called %q, want %q", called.Load(), test.want)
			}
		})
	}
	if _, err := os.Stat(os.Getenv("KUBECONFIG")); !os.IsNotExist(err) {
		t.Fatal("API administration touched the kubeconfig")
	}
}
