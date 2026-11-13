package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestRunLogDefaults(t *testing.T) {
	for _, test := range []struct {
		name, failedStage, failedStep, stage, step, wantStep, wantError string
		multi                                                           bool
	}{
		{name: "failed step in multi-step burn", failedStage: "test", failedStep: "unit", multi: true, wantStep: "unit"},
		{name: "explicit step", failedStage: "test", failedStep: "integration", step: "unit", multi: true, wantStep: "unit"},
		{name: "explicit burn only", failedStage: "test", failedStep: "integration", stage: "setup", wantStep: "prepare"},
		{name: "single step", stage: "test", wantStep: "unit"},
		{name: "failed burn only", failedStage: "test", wantStep: "unit"},
		{name: "multi-step needs step", stage: "test", multi: true, wantError: "step is required"},
		{name: "explicit burn does not infer failed step", failedStage: "test", failedStep: "unit", stage: "test", multi: true, wantError: "step is required"},
		{name: "no failed burn", wantError: "available burn/step pairs: setup/prepare, test/unit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRetentionFixture(t)
			ctx := t.Context()
			enqueued, err := storetest.EnqueueRun(f.retention.Database, ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch,
				Ref: "feature/defaults", SHA: strings.Repeat("a", 40), Actor: "reader", Trigger: "branch"})
			if err != nil {
				t.Fatal(err)
			}
			run := enqueued.Run
			steps := []model.StepResult{{Stage: "test", Step: "unit"}, {Stage: "setup", Step: "prepare"}}
			if test.multi {
				steps = append(steps, model.StepResult{Stage: "test", Step: "integration"})
			}
			for i, step := range steps {
				step.RunID, step.Status, step.Ordinal = run.ID, model.StepPassed, i
				if _, err := f.retention.Database.PutStepResult(ctx, step); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.retention.Database.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunFailed, FailedStage: test.failedStage, FailedStep: test.failedStep}); err != nil {
				t.Fatal(err)
			}
			file, err := f.retention.Logs.Create(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString("[test/unit] unit output\n[setup/prepare] prepare output\n[test/integration] integration output\n"); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := f.retention.Logs.BuildIndex(run.ID); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]string{"id": run.ID[:8], "burn": test.stage, "step": test.step})
			value, err := f.api.CallTool(ctx, api.Actor{Identity: "reader"}, "run_logs", raw)
			if test.wantError != "" {
				if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("got %v, want %s", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := value.(LogResponse)
			if got.RunID != run.ID || got.Step != test.wantStep || !strings.Contains(got.Output, test.wantStep+" output") {
				t.Fatalf("log = %#v", got)
			}
		})
	}
}

func TestRunPrefixArtifactsWaitAndHTTP(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "feature/prefix", "branch", true)
	actor := api.Actor{Identity: "reader"}
	for _, id := range []string{run.ID[:8], run.ID[:12], run.ID} {
		for _, tool := range []string{"artifacts", "artifact_get", "wait"} {
			args := map[string]string{"id": id}
			if tool == "artifact_get" {
				args["name"] = "report.txt"
			}
			raw, _ := json.Marshal(args)
			value, err := f.api.CallTool(t.Context(), actor, tool, raw)
			if err != nil {
				t.Fatalf("%s(%s): %v", tool, id, err)
			}
			body, _ := json.Marshal(value)
			if !strings.Contains(string(body), run.ID) {
				t.Fatalf("%s resolved wrong run: %s", tool, body)
			}
		}
	}
	server, err := api.New(runSurfaceAuth{}, f.api, f.api, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "/logs?burn=test", "/artifacts", "/artifacts/report.txt"} {
		request := httptest.NewRequest(http.MethodGet, "/api/runs/"+run.ID[:8]+path, nil)
		request.Header.Set("Authorization", "Bearer test")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("HTTP %s: %d %s", path, response.Code, response.Body)
		}
	}
}

func TestRunPrefixScopeAcrossTools(t *testing.T) {
	f := newScopeFixture(t)
	f.service.runReruns, f.service.enqueues = f.store, &retryEnqueueObserver{}
	for _, tool := range []string{"run_get", "run_logs", "run_cancel", "run_rerun", "wait", "artifacts", "artifact_get"} {
		for _, size := range []int{8, 32} {
			var messages []string
			for _, id := range []string{f.runs["codeberg"][0].ID[:size], strings.Repeat("f", size)} {
				args := map[string]string{"id": id}
				if tool == "artifact_get" {
					args["name"] = "report.txt"
				}
				value, err := f.call(scopedActor, tool, args)
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("%s(%s): %v", tool, id, err)
				}
				f.assertNoLeak(t, tool, value, err, id)
				messages = append(messages, strings.ReplaceAll(err.Error(), id, "ID"))
			}
			if messages[0] != messages[1] {
				t.Fatalf("%s hidden and missing errors differ: %v", tool, messages)
			}
		}
	}
	for _, id := range []string{f.runs["github"][0].ID[:8], f.runs["github"][0].ID} {
		value, err := f.call(scopedActor, "wait", map[string]string{"id": id})
		if err != nil || value.(WaitResponse).Run.ID != f.runs["github"][0].ID {
			t.Fatalf("scoped wait: %v, %v", value, err)
		}
	}
}

func TestScopedPrefixesNeverUseGlobalResolverFallback(t *testing.T) {
	f := newScopeFixture(t)
	// Hide the store's scoped-resolution capability. Its global resolver
	// reports ambiguity, which must never become a scoped existence oracle.
	f.service.history = ambiguousRunHistory{RunHistory: f.store}
	for _, id := range []string{f.runs["github"][0].ID[:8], f.runs["codeberg"][0].ID[:8], strings.Repeat("f", 8)} {
		if _, err := f.call(scopedActor, "run_get", map[string]string{"id": id}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("fallback prefix %s: %v", id, err)
		}
	}
	id := f.runs["github"][0].ID
	if _, err := f.call(scopedActor, "run_get", map[string]string{"id": id}); err != nil {
		t.Fatalf("fallback full ID: %v", err)
	}
}

func TestRunLogValidationNamesFieldAndType(t *testing.T) {
	f := newRetentionFixture(t)
	for _, test := range []struct{ field, value, expected string }{
		{"tail", `10`, "boolean"}, {"tail", `"true"`, "boolean"},
		{"limit", `true`, "line count"}, {"limit", `"20"`, "line count"},
		{"limit", `-1`, "line count"}, {"offset", `-1`, "line count"},
		{"context", `"2"`, "line count"}, {"id", `123`, "string"},
	} {
		raw := json.RawMessage(fmt.Sprintf(`{"id":"12345678",%q:%s}`, test.field, test.value))
		_, err := f.api.CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_logs", raw)
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), test.field) || !strings.Contains(err.Error(), test.expected) {
			t.Errorf("%s: %v, want %s and %s", raw, err, test.field, test.expected)
		}
	}
}

func TestWaitByRunIDPinsOriginalAttempt(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "feature/wait", "branch", false)
	raw, _ := json.Marshal(map[string]string{"id": run.ID[:8]})
	wait, err := f.api.PrepareLongPoll(t.Context(), api.Actor{Identity: "reader"}, "wait", raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.retention.Database.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunInterrupted}); err != nil {
		t.Fatal(err)
	}
	f.run(t, "feature/wait", "branch", true)
	value, err := wait(t.Context())
	if err != nil || value.(WaitResponse).Run.ID != run.ID || value.(WaitResponse).StillRunning {
		t.Fatalf("wait followed a later attempt: %#v, %v", value, err)
	}
}

type ambiguousRunHistory struct{ RunHistory }

func (h ambiguousRunHistory) ResolveRunID(context.Context, string) (model.Run, error) {
	return model.Run{}, store.ErrAmbiguous
}
