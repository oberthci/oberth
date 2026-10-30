package service

import (
	"bytes"
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
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type blockedRunHistory struct{ RunHistory }

func (blockedRunHistory) ResolveRunID(ctx context.Context, _ string) (model.Run, error) {
	<-ctx.Done()
	return model.Run{}, ctx.Err()
}

func TestRunReadsBoundDatabaseWaitAndPreserveEarlierDeadline(t *testing.T) {
	for _, tool := range []string{"run_get", "run_logs"} {
		for _, budget := range []time.Duration{2 * time.Second, time.Minute} {
			t.Run(tool+budget.String(), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					service := &API{history: blockedRunHistory{}, logs: struct{ LogStore }{}}
					ctx, cancel := context.WithTimeout(t.Context(), budget)
					defer cancel()
					started := time.Now()
					value, err := service.CallTool(ctx, api.Actor{Identity: "reader"}, tool, json.RawMessage(`{"id":"aaaaaaaa"}`))
					if value != nil || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrUnavailable) || time.Since(started) != min(budget, runReadBudget) {
						t.Fatalf("unbounded read or lost error classification: value=%+v err=%v duration=%s", value, err, time.Since(started))
					}
					if strings.Count(err.Error(), "run read budget exhausted") != 1 || strings.Count(err.Error(), "unavailable") != 1 {
						t.Fatalf("deadline refusal wrapped twice: %v", err)
					}
				})
			})
		}
	}
}

type blockedRunScope struct{ RunResolver }

func (blockedRunScope) ListUpstreams(ctx context.Context) ([]model.Upstream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockedRunScope) ListRepositories(context.Context) ([]model.Repository, error) {
	panic("scope query continued after deadline")
}

func TestRunReadBudgetIncludesScopeResolution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := &API{runs: blockedRunScope{}}
		started := time.Now()
		_, err := service.CallTool(t.Context(), api.Actor{Identity: "scoped", Scope: []string{"allowed"}}, "run_get", json.RawMessage(`{"id":"aaaaaaaa"}`))
		if !errors.Is(err, ErrUnavailable) || time.Since(started) != runReadBudget {
			t.Fatalf("scope lookup escaped read budget: %v after %s", err, time.Since(started))
		}
	})
}

func TestRunLogsBoundedMetadataAndByteCursorReachCaller(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "feature/bounded", "branch", true)
	file, err := os.OpenFile(filepath.Join(f.root, "logs", run.ID+".log"), os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	line := "[test/unit] match " + strings.Repeat("x", 100) + "\n"
	if _, err := file.WriteString(strings.Repeat(line, 10000)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.retention.Logs.BuildIndex(run.ID); err != nil {
		t.Fatal(err)
	}
	var cursor int64
	for range 2 {
		raw, _ := json.Marshal(map[string]any{"id": run.ID, "burn": "test", "step": "unit", "limit": 1, "byte_offset": cursor})
		value, err := f.api.CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_logs", raw)
		if err != nil {
			t.Fatal(err)
		}
		got := value.(LogResponse)
		if got.Output != line || got.ScanComplete || !got.HasMore || !got.BytesExact || got.WithheldBytes == 0 || got.NextByteOffset != cursor+int64(len(line)) {
			t.Fatalf("lost bounded metadata: %+v", got)
		}
		if text := got.MCPToolText(); !strings.HasPrefix(text, line) || !strings.Contains(text, "[Log continuation:") || got.Output != line {
			t.Fatalf("nonempty page lost raw output or continuation: %q", text)
		}
		cursor = got.NextByteOffset
	}
}

func TestRunLogsEmptyActiveWindowHasMCPContinuation(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "feature/empty-window", "branch", false)
	if _, err := f.retention.Database.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.retention.Database.PutStepResult(t.Context(), model.StepResult{RunID: run.ID, Burn: "test", Step: "unit", Status: model.StepPassed, DeclaredSize: "M"}); err != nil {
		t.Fatal(err)
	}
	wanted := "[test/unit] actual error\n"
	raw := strings.Repeat("[other/step] noise\n", 360000) + wanted
	if err := os.WriteFile(filepath.Join(f.root, "logs", run.ID+".log"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := api.New(runSurfaceAuth{}, f.api, f.api, "test")
	if err != nil {
		t.Fatal(err)
	}
	var cursor int64
	for page := 0; page < 2; page++ {
		args := map[string]any{"id": run.ID, "burn": "test", "step": "unit", "byte_offset": cursor}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "run_logs", "arguments": args}})
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		var rpc struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				StructuredContent LogResponse `json:"structuredContent"`
				IsError           bool        `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &rpc); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || rpc.Result.IsError || len(rpc.Result.Content) != 1 {
			t.Fatalf("MCP failed: %d %s", response.Code, response.Body)
		}
		got, text := rpc.Result.StructuredContent, rpc.Result.Content[0].Text
		if page == 0 {
			if got.Output != "" || !got.HasMore || got.NextByteOffset <= 0 || !strings.Contains(text, "No lines in this bounded window") || !strings.Contains(text, fmt.Sprintf("byte_offset=%d", got.NextByteOffset)) || !strings.Contains(text, "offset=0") {
				t.Fatalf("empty page lacks continuation: %+v text=%q", got, text)
			}
			cursor = got.NextByteOffset
		} else if got.Output != wanted || text != wanted || got.HasMore {
			t.Fatalf("continuation failed: %+v text=%q", got, text)
		}
	}
}
