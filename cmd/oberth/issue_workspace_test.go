package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestIssuesCLIUpdatesWorkspaceStatusViaMCP(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct {
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Method != "tools/call" || request.Params.Name != "issue_update" || request.Params.Arguments["id"] != float64(42) || request.Params.Arguments["work_status"] != "active" || request.Params.Arguments["expected_revision"] != float64(7) {
			t.Errorf("unexpected mutation: %+v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}],"structuredContent":{"id":42,"state":"open","revision":8}}}`))
	}))
	defer server.Close()
	configure(t, server)
	t.Setenv("OBERTH_MCP_URL", server.URL+"/mcp")
	var out bytes.Buffer
	if err := runIssues(t.Context(), []string{"update", "--id", "42", "--work-status", "active", "--expected-revision", "7"}, &out); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	for _, args := range [][]string{{"update", "--id", "42", "--work-status", "bogus"}, {"update", "--id", "0", "--work-status", "active"}, {"--stale-after", "-1m"}, {"--work-status", "bogus"}} {
		if err := runIssues(t.Context(), args, &out); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if calls != 1 {
		t.Fatal("invalid input reached server")
	}
}

func TestServeWorkspaceStaleFlags(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--issue-stale-after", "0s"},
		{"--issue-active-stale-after", "-1m"},
		{"--issue-stale-after", "1h", "--issue-active-stale-after", "2h"},
	} {
		if _, err := parseServeOptions(args, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	options, err := parseServeOptions([]string{"--argo-namespace", "argo", "--issue-stale-after", "2h", "--issue-active-stale-after", "10m"}, io.Discard)
	if err != nil || options.issueStaleAfter != 2*time.Hour || options.issueActiveStaleAfter != 10*time.Minute {
		t.Fatalf("options=%+v err=%v", options, err)
	}
}

func TestWorkspaceStatusMCPAndHTTP(t *testing.T) {
	database, err := storetest.Open(t.Context(), filepath.Join(t.TempDir(), "issues.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	issue, err := database.CreateManualIssue(t.Context(), "agent@host", model.ManualIssueSpec{Title: "workspace", Body: "progress"})
	if err != nil {
		t.Fatal(err)
	}
	control, err := service.NewAPI(service.APIConfig{Runs: database, Issues: database})
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.New(toolingAuthenticator{}, control, control, "test", api.WithErrorClassifier(classifyViewError))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body string
		want               []string
	}{
		{http.MethodPost, "/mcp", fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"issue_update","arguments":{"id":%d,"work_status":"active","expected_revision":1}}}`, issue.ID), []string{`"revision":2`}},
		{http.MethodPost, "/mcp", fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"issue_get","arguments":{"id":%d,"stale_after":"1ns","active_stale_after":"1ns"}}}`, issue.ID), []string{`"work_status":"active"`, `"stale":true`, `"structuredContent"`}},
		{http.MethodGet, "/api/issues?work_status=active&stale_after=1ns&active_stale_after=1ns", "", []string{`"work_status":"active"`, `"stale":true`, `"total":1`}},
		{http.MethodGet, "/api/issues?work_status=review", "", []string{`"total":0`}},
	} {
		request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		request.Header.Set("Authorization", "Bearer fixture")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"isError":true`) {
			t.Fatalf("%s: %d %s", tc.path, response.Code, response.Body)
		}
		for _, want := range tc.want {
			if !strings.Contains(response.Body.String(), want) {
				t.Errorf("%s missing %s: %s", tc.path, want, response.Body)
			}
		}
	}
}
