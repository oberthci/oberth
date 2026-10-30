package service

import (
	"context"
	"encoding/json"
	"errors"
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
		cursor = got.NextByteOffset
	}
}
