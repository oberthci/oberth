package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

type toolingAuthenticator struct{}

func (toolingAuthenticator) Authenticate(context.Context, string) (api.Actor, error) {
	return api.Actor{Identity: "tooling-test@host"}, nil
}

// Exercise the production MCP handler and error classifier, not only the Go
// error: pending logs and malformed IDs must be useful to text-only clients.
func TestMCPRunToolsExposePendingAndInvalidIDs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(root, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "origin", Kind: "forgejo", BaseURL: "https://forge.example.test/org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/tooling", SHA: strings.Repeat("a", 40), Trigger: "branch", Actor: "tooling-test@host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	logs, err := runlog.Open(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := logs.AppendStepProgress(run.ID, runprogress.Event{Version: runprogress.Version, Burn: "release-build-variants", Step: "release-build-variants", Status: runprogress.StepRunning, StartedAt: &started}); err != nil {
		t.Fatal(err)
	}
	control, err := service.NewAPI(service.APIConfig{Runs: database, History: database, Repositories: database, Logs: logs})
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.New(toolingAuthenticator{}, control, control, "test", api.WithErrorClassifier(classifyViewError))
	if err != nil {
		t.Fatal(err)
	}
	call := func(tool, arguments string) (bool, string) {
		t.Helper()
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool, arguments)
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer test-token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("MCP status %d: %s", response.Code, response.Body)
		}
		var envelope struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if len(envelope.Result.Content) != 1 {
			t.Fatalf("MCP result = %s", response.Body)
		}
		return envelope.Result.IsError, envelope.Result.Content[0].Text
	}
	isError, text := call("run_logs", fmt.Sprintf(`{"id":%q,"burn":"release-build-variants","step":"release-build-variants"}`, run.ID[:12]))
	if !isError || !strings.Contains(text, "step log is pending") || !strings.Contains(text, run.ID) || !strings.Contains(text, "release-build-variants") || !strings.Contains(text, "retry") || strings.Contains(text, root) || strings.Contains(text, "internal error") {
		t.Fatalf("pending MCP text is not actionable: %q (isError=%t)", text, isError)
	}
	isError, text = call("run_get", fmt.Sprintf(`{"id":%q}`, run.ID[:12]))
	if isError || !strings.Contains(text, run.ID) {
		t.Fatalf("short run ID did not roundtrip: %q", text)
	}
	isError, text = call("run_get", `{"id":"abc123"}`)
	if !isError || !strings.Contains(text, "12 to 32 hexadecimal") || strings.Contains(text, "internal error") {
		t.Fatalf("invalid ID error = %q", text)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/runs/"+run.ID[:12]+"/logs?burn=release-build-variants&step=release-build-variants", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "step log is pending") {
		t.Fatalf("dashboard pending response = %d %s", response.Code, response.Body)
	}
}

func TestMCPBothLogRoutesBoundActualFilteredTailLines(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(t.Context(), filepath.Join(root, "db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	u, err := db.CreateUpstream(t.Context(), model.UpstreamSpec{Name: "origin", Kind: "forgejo", BaseURL: "https://forge.example.test/org"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := db.CreateRepository(t.Context(), model.RepositorySpec{Name: "repo", UpstreamID: u.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.EnqueueRun(t.Context(), model.RunSpec{RepoID: r.ID, RefKind: model.RefBranch, Ref: "feature/logs", SHA: strings.Repeat("a", 40), Trigger: "branch", Actor: "test@host"})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := runlog.Open(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	if err := logs.WriteStepPlan(run.ID, []runprogress.PlannedStep{{Burn: "test", Step: "test"}}); err != nil {
		t.Fatal(err)
	}
	f, err := logs.Create(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if _, err := fmt.Fprintf(f, "[test/test] PASS %d\n[test/test] ok %d\n[test/test] context %d\n[other/other] FAIL elsewhere\n", i, i, i); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.BuildIndex(run.ID); err != nil {
		t.Fatal(err)
	}
	control, err := service.NewAPI(service.APIConfig{Runs: db, History: db, Repositories: db, Logs: logs})
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.New(toolingAuthenticator{}, control, control, "test", api.WithErrorClassifier(classifyViewError))
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"logs", "run_logs"} {
		for _, contextLines := range []int{0, 2} {
			args := fmt.Sprintf(`"repo":"repo","sha":%q,"step":"test"`, run.SHA)
			if tool == "run_logs" {
				args = fmt.Sprintf(`"id":%q,"burn":"test","step":"test"`, run.ID)
			}
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{%s,"pattern":"^(ok|PASS|FAIL)","context":%d,"limit":8,"tail":true}}}`, tool, args, contextLines)
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer fixture")
			out := httptest.NewRecorder()
			server.Handler().ServeHTTP(out, req)
			var response struct {
				Result struct {
					IsError    bool `json:"isError"`
					Structured struct {
						Output  string `json:"output"`
						Content string `json:"content"`
						runlog.Meta
					} `json:"structuredContent"`
				} `json:"result"`
			}
			if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			got := response.Result.Structured
			if response.Result.IsError || got.ReturnedLines != 8 || !got.Truncated || got.TotalLines != 90 || got.MatchedLines != 60 {
				t.Fatalf("%s context%d: %s", tool, contextLines, out.Body)
			}
			content := got.Output + got.Content
			if strings.Count(content, "\n") != 8 || strings.Contains(content, "elsewhere") || !strings.Contains(content, "29") {
				t.Fatalf("%s context%d invalid output %q: %s", tool, contextLines, content, out.Body)
			}
		}
	}
}
