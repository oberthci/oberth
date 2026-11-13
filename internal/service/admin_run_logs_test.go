package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestAdminRunLogsResolvesUniqueStepBeforeFailedBurnDefault(t *testing.T) {
	f := newControlFixture(t)
	run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/logs", SHA: strings.Repeat("a", 40), Actor: "dev", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []model.StepResult{{RunID: run.ID, Stage: "build", Step: "compile", Status: model.StepPassed}, {RunID: run.ID, Stage: "unit", Step: "check", Status: model.StepFailed}, {RunID: run.ID, Stage: "integration", Step: "check", Status: model.StepFailed}} {
		if _, err = storetest.PutHistoricalStepResult(f.store, t.Context(), step); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = f.store.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunFailed, FailedStage: "unit", FailedStep: "check"}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.root, "logs", run.ID+".log"), []byte("[build/compile] compiled\n[unit/check] failed\n[integration/check] failed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = f.logs.BuildIndex(run.ID); err != nil {
		t.Fatal(err)
	}
	control := f.api(t)
	value, err := control.CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_logs", json.RawMessage(fmt.Sprintf(`{"id":%q}`, run.ID)))
	if err != nil || value.(LogResponse).Stage != "unit" || value.(LogResponse).Step != "check" || len(value.(LogResponse).AvailablePairs) != 0 {
		t.Fatalf("failed default changed: %+v %v", value, err)
	}
	value, err = control.CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_logs", json.RawMessage(fmt.Sprintf(`{"id":%q,"step":"compile"}`, run.ID)))
	if err != nil {
		t.Fatal(err)
	}
	if got := value.(LogResponse); got.Stage != "build" || got.Step != "compile" {
		t.Fatalf("wrong unique step: %+v", got)
	}
	_, err = control.CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_logs", json.RawMessage(fmt.Sprintf(`{"id":%q,"step":"check"}`, run.ID)))
	if err == nil || !strings.Contains(err.Error(), "integration") || !strings.Contains(err.Error(), "unit") {
		t.Fatalf("ambiguous step silently selected failed burn: %v", err)
	}
}

func TestAdminRunLogsStepInferenceUsesActiveProgress(t *testing.T) {
	f := newControlFixture(t)
	run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/live-log", SHA: strings.Repeat("b", 40), Actor: "dev", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err = f.logs.AppendStepProgress(run.ID, runprogress.Event{Version: runprogress.Version, Stage: "build", Step: "compile", Status: runprogress.StepRunning, StartedAt: &started}); err != nil {
		t.Fatal(err)
	}
	_, err = f.api(t).CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_logs", json.RawMessage(fmt.Sprintf(`{"id":%q,"step":"compile"}`, run.ID)))
	if err == nil || !strings.Contains(err.Error(), "step log is pending") || !strings.Contains(err.Error(), "build") {
		t.Fatalf("live unique step=%v", err)
	}
}

func TestRunLogsPassedDefaultsToFirstStepAndListsAvailablePairs(t *testing.T) {
	for _, count := range []int{1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newControlFixture(t)
			run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/logs", SHA: strings.Repeat("a", 40), Actor: "dev", Trigger: "push"})
			if err != nil {
				t.Fatal(err)
			}
			steps := []model.StepResult{{RunID: run.ID, Stage: "z-build", Step: "compile", Ordinal: 0, Status: model.StepPassed}, {RunID: run.ID, Stage: "a-test", Step: "unit", Ordinal: 1, Status: model.StepPassed}}
			for _, step := range steps[:count] {
				if _, err = storetest.PutHistoricalStepResult(f.store, t.Context(), step); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = f.store.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(f.root, "logs", run.ID+".log"), []byte("[z-build/compile] first step output\n[a-test/unit] other step output\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = f.logs.BuildIndex(run.ID); err != nil {
				t.Fatal(err)
			}
			value, err := f.api(t).CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_logs", json.RawMessage(fmt.Sprintf(`{"id":%q}`, run.ID)))
			if err != nil {
				t.Fatal(err)
			}
			got := value.(LogResponse)
			if got.Stage != "z-build" || got.Step != "compile" || !strings.Contains(got.Output, "first step output") || strings.Contains(got.Output, "other step output") || len(got.AvailablePairs) != count || got.AvailablePairs[0] != "z-build/compile" {
				t.Fatalf("wrong default: %+v", got)
			}
			if count == 2 {
				value, err = f.api(t).CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_logs", json.RawMessage(fmt.Sprintf(`{"id":%q,"step":"unit"}`, run.ID)))
				if err != nil || value.(LogResponse).Stage != "a-test" {
					t.Fatalf("explicit step: %+v %v", value, err)
				}
			}
		})
	}
}
