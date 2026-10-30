package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
func TestMCPRunToolsExposePendingNotExecutedAndInvalidIDs(t *testing.T) {
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
	control, err := service.NewAPI(service.APIConfig{Runs: database, History: database, Repositories: database, Logs: logs, KVMPreflights: database})
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
				IsError    bool                `json:"isError"`
				Structured service.LogResponse `json:"structuredContent"`
				Content    []struct {
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
		if got := envelope.Result.Structured; (tool == "run_logs" || tool == "logs") && !envelope.Result.IsError {
			want := service.LogResponse{RunID: run.ID, Burn: "release-build-variants", Step: "release-build-variants",
				Note: "step was not executed; no log recorded", RunError: "Workflow error"}
			if got != want {
				t.Fatalf("MCP log metadata = %#v, want %#v", got, want)
			}
		}
		return envelope.Result.IsError, envelope.Result.Content[0].Text
	}
	isError, text := call("run_logs", fmt.Sprintf(`{"id":%q,"burn":"release-build-variants","step":"release-build-variants"}`, run.ID[:8]))
	if !isError || !strings.Contains(text, "step log is pending") || !strings.Contains(text, run.ID) || !strings.Contains(text, "release-build-variants") || !strings.Contains(text, "retry") || strings.Contains(text, root) || strings.Contains(text, "internal error") {
		t.Fatalf("pending MCP text is not actionable: %q (isError=%t)", text, isError)
	}
	isError, text = call("run_get", fmt.Sprintf(`{"id":%q}`, run.ID[:8]))
	if isError || !strings.Contains(text, run.ID) {
		t.Fatalf("short run ID did not roundtrip: %q", text)
	}
	isError, text = call("run_get", `{"id":"abc123"}`)
	if !isError || !strings.Contains(text, "8 to 32 hexadecimal") || strings.Contains(text, "internal error") {
		t.Fatalf("invalid ID error = %q", text)
	}
	isError, text = call("run_kvm_preflight", fmt.Sprintf(`{"id":%q}`, run.ID[:8]))
	var preflight service.KVMPreflightResponse
	if isError || json.Unmarshal([]byte(text), &preflight) != nil || preflight.Present || preflight.Evidence != nil || preflight.RunID != run.ID {
		t.Fatalf("KVM absent historical evidence did not roundtrip: %q", text)
	}
	isError, text = call("run_kvm_preflight", `{"id":"abc123"}`)
	if !isError || !strings.Contains(text, "8 to 32 hexadecimal") {
		t.Fatalf("invalid KVM run ID error = %q", text)
	}
	unauthenticated := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"run_kvm_preflight","arguments":{"id":"`+run.ID+`"}}}`))
	denied := httptest.NewRecorder()
	server.Handler().ServeHTTP(denied, unauthenticated)
	if denied.Code != http.StatusUnauthorized || strings.Contains(denied.Body.String(), run.ID) {
		t.Fatal("KVM evidence exposed without bearer authentication")
	}
	request := httptest.NewRequest(http.MethodGet, "/api/runs/"+run.ID[:8]+"/logs?burn=release-build-variants&step=release-build-variants", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "step log is pending") {
		t.Fatalf("dashboard pending response = %d %s", response.Code, response.Body)
	}

	// A workflow failure can leave a failed step that never had a pod or log
	// marker. Exercise both wire formats with no file, an empty file, and a
	// finalized index containing only another step's output.
	if _, err := database.PutStepResult(ctx, model.StepResult{
		RunID: run.ID, Burn: "release-build-variants", Step: "release-build-variants",
		Status: model.StepFailed, DeclaredSize: "M", Termination: model.UnavailableStepDiagnostic("not-executed"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.FinishRun(ctx, run.ID, model.RunResult{
		Status: model.RunFailed, FailedBurn: "release-build-variants", FailedStep: "release-build-variants", Error: "Workflow error",
	}); err != nil {
		t.Fatal(err)
	}
	want := service.LogResponse{RunID: run.ID, Burn: "release-build-variants", Step: "release-build-variants",
		Note: "step was not executed; no log recorded", RunError: "Workflow error"}
	for _, storage := range []string{"missing", "empty", "other-step"} {
		if storage != "missing" {
			content := ""
			if storage == "other-step" {
				content = "[setup/prepare] other step output\n"
			}
			if err := os.WriteFile(filepath.Join(root, "logs", run.ID+".log"), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := logs.BuildIndex(run.ID); err != nil {
				t.Fatal(err)
			}
		}
		for _, filter := range []struct{ arguments, query string }{
			{"", ""}, {`,"tail":true`, "&tail=true"},
			{`,"pattern":"FAIL","context":2`, "&pattern=FAIL&context=2"},
			{`,"offset":10,"limit":5`, "&offset=10&limit=5"},
			{`,"tail":true,"pattern":"FAIL","context":2,"offset":10,"limit":5`, "&tail=true&pattern=FAIL&context=2&offset=10&limit=5"},
		} {
			for _, tool := range []string{"run_logs", "logs"} {
				args := fmt.Sprintf(`"id":%q,"step":"release-build-variants"`, run.ID[:8])
				if tool == "logs" {
					args = fmt.Sprintf(`"repo":"repo","sha":%q,"step":"release-build-variants"`, run.SHA)
				}
				isError, text := call(tool, "{"+args+filter.arguments+"}")
				if isError || text != want.MCPToolText() {
					t.Fatalf("%s %s %s: isError=%t text=%q", storage, tool, filter.arguments, isError, text)
				}
			}
			request := httptest.NewRequest(http.MethodGet, "/api/runs/"+run.ID[:8]+"/logs?step=release-build-variants"+filter.query, nil)
			request.Header.Set("Authorization", "Bearer test-token")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			var got service.LogResponse
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || got != want {
				t.Fatalf("%s %s: HTTP %d %s", storage, filter.query, response.Code, response.Body)
			}
		}
	}

	remote := httptest.NewServer(server.Handler())
	t.Cleanup(remote.Close)
	configure(t, remote)
	for _, flags := range [][]string{nil, {"--raw"}, {"--tail", "--pattern", "FAIL", "--offset", "10"}} {
		var out bytes.Buffer
		args := append([]string{run.ID, "--burn", want.Burn, "--step", want.Step}, flags...)
		if err := runRemoteLog(ctx, args, &out); err != nil {
			t.Fatal(err)
		}
		if out.String() != want.MCPToolText()+"\n" {
			t.Fatalf("CLI %v: %q", flags, out.String())
		}
	}

	// Unexpected missing output is still a storage error. Neither a zero
	// range alone nor a not-executed diagnostic with a real range qualifies.
	for _, step := range []model.StepResult{
		{Termination: model.UnavailableStepDiagnostic("not-recorded")},
		{Termination: model.UnavailableStepDiagnostic("not-executed"), LogEnd: 1},
	} {
		step.RunID, step.Burn, step.Step = run.ID, want.Burn, want.Step
		step.Status, step.DeclaredSize = model.StepFailed, "M"
		if _, err := database.PutStepResult(ctx, step); err != nil {
			t.Fatal(err)
		}
		isError, text := call("run_logs", fmt.Sprintf(`{"id":%q,"step":%q}`, run.ID, want.Step))
		if !isError || !strings.Contains(text, "internal error") {
			t.Fatalf("unexpected missing log was hidden: isError=%t text=%q", isError, text)
		}
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

func TestRunCursorHTTPErrorExplainsHowToResume(t *testing.T) {
	ctx := t.Context()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "cursor.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	control, err := service.NewAPI(service.APIConfig{Runs: database, History: database, Repositories: database})
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.New(toolingAuthenticator{}, control, control, "test", api.WithErrorClassifier(classifyViewError))
	if err != nil {
		t.Fatal(err)
	}
	for _, cursor := range []string{"123", strings.Repeat("a", 32)} {
		request := httptest.NewRequest(http.MethodGet, "/api/runs?before="+cursor, nil)
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "cursor run no longer retained; restart from the first page") || strings.Contains(response.Body.String(), cursor) {
			t.Fatalf("cursor %s: %d %s", cursor, response.Code, response.Body)
		}
	}
}
