package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type runSurfaceAuth struct{}

func (runSurfaceAuth) Authenticate(context.Context, string) (api.Actor, error) {
	return api.Actor{Identity: "agent@host"}, nil
}

func TestRunListMCPMatchesHTTPOrderAndPassedAlias(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	reader := f.api(t)
	var passed []model.Run
	for i, release := range []bool{false, true} {
		spec := model.RunSpec{RepoID: f.repo.ID, Ref: "feature/surface", RefKind: model.RefBranch, SHA: strings.Repeat(string(rune('a'+i)), 40), Trigger: "push", Actor: "agent@host", Release: release}
		if release {
			spec.Ref = "v1.0.0"
			spec.RefKind = model.RefTag
		}
		run, err := f.store.EnqueueRun(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.MarkRunAdmitted(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		finished, err := f.store.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed})
		if err != nil {
			t.Fatal(err)
		}
		passed = append(passed, finished)
	}
	queued, err := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: "surface-queue", Repository: f.repo, Branch: "feature/queued", SHA: strings.Repeat("c", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.New(runSurfaceAuth{}, reader, reader, "test")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/runs?limit=2", nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var rows []model.Run
	if err = json.Unmarshal(response.Body.Bytes(), &rows); err != nil || response.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s: %v", response.Code, response.Body, err)
	}
	value, err := reader.CallTool(ctx, api.Actor{Identity: "agent@host"}, "run_list", json.RawMessage(`{"limit":2}`))
	if err != nil {
		t.Fatal(err)
	}
	toolRows := value.(api.RunListResponse).Runs.([]model.Run)
	if len(rows) != 3 || len(toolRows) != len(rows) || rows[0].ID != queued.ID || rows[1].ID != passed[1].ID || rows[2].ID != passed[0].ID {
		t.Fatalf("HTTP=%+v MCP=%+v", rows, toolRows)
	}
	for i := range rows {
		if rows[i].ID != toolRows[i].ID {
			t.Fatalf("order differs at %d", i)
		}
	}
	for _, run := range passed {
		for _, tool := range []string{"status", "wait"} {
			args, _ := json.Marshal(map[string]any{"repo": f.repo.Name, "ref": run.SHA})
			if tool == "wait" {
				args, _ = json.Marshal(map[string]any{"repo": f.repo.Name, "sha": run.SHA})
			}
			result, err := reader.CallTool(ctx, api.Actor{Identity: "agent@host"}, tool, args)
			if err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(result)
			var got struct {
				Status        string `json:"status"`
				DisplayStatus string `json:"display_status"`
			}
			if err = json.Unmarshal(payload, &got); err != nil || got.Status != "delivered" || got.DisplayStatus != "passed" {
				t.Fatalf("%s release=%t: %s, %v", tool, run.Release, payload, err)
			}
		}
	}
}
