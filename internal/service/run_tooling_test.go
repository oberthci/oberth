package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
)

// #553: the 12-character ID printed by push feedback must be directly usable.
func TestRunGetAcceptsPushFeedbackID(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	run, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "short-run-id", Repository: fixture.repo,
		Branch: "feature/short-run-id", SHA: strings.Repeat("a", 40), Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{run.ID, run.ID[:12]} {
		value, err := fixture.api(t).CallTool(ctx, api.Actor{Identity: "agent@host"}, "run_get",
			json.RawMessage(fmt.Sprintf(`{"id":%q}`, id)))
		if err != nil {
			t.Fatalf("run_get(%q): %v", id, err)
		}
		detail := value.(RunDetailResponse)
		if detail.Run.ID != run.ID || detail.Repository.ID != fixture.repo.ID {
			t.Fatalf("run_get(%q) = %#v", id, detail)
		}
	}
}

// #553: Argo records progress before it replays a completed pod's output.
// A real running step can have neither a retained range nor even a log file.
func TestActiveRunLogsWithoutRetainedOutputArePending(t *testing.T) {
	for _, createLog := range []bool{false, true} {
		t.Run(fmt.Sprintf("log-file-%t", createLog), func(t *testing.T) {
			fixture := newControlFixture(t)
			ctx := context.Background()
			run, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
				EventID: "pending-run-log", Repository: fixture.repo,
				Branch: "feature/pending-run-log", SHA: strings.Repeat("b", 40), Actor: "agent@host",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
				t.Fatal(err)
			}
			started := time.Now().UTC()
			if err := fixture.logs.AppendStepProgress(run.ID, runprogress.Event{
				Version: runprogress.Version, Burn: "release-build-variants", Step: "release-build-variants",
				Status: runprogress.StepRunning, StartedAt: &started,
			}); err != nil {
				t.Fatal(err)
			}
			if createLog {
				file, err := fixture.logs.Create(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString("[setup/prepare] other step output\n"); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			control := fixture.api(t)
			arguments := json.RawMessage(fmt.Sprintf(`{"id":%q,"burn":"release-build-variants","step":"release-build-variants","pattern":"Building variant|vmlinuz size|FATAL|error:|built in","limit":20}`, run.ID[:12]))
			_, err = control.CallTool(ctx, api.Actor{Identity: "agent@host"}, "run_logs", arguments)
			if !errors.Is(err, ErrStepLogPending) {
				t.Fatalf("active run_logs returned %v, want an actionable pending error", err)
			}
			if _, err := control.RunLog(ctx, api.Actor{Identity: "agent@host"}, run.ID, "unknown", "release-build-variants"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown burn/step error = %v, want not found", err)
			}
			if _, err := os.Stat(filepath.Join(fixture.root, "logs", run.ID+".index.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("active read created a terminal index: %v", err)
			}
			// Once the controller retains the step's output and result, the
			// same request returns its normal filtered bytes and canonical ID.
			output := "[release-build-variants/release-build-variants] Building variant minimal\n"
			if err := os.WriteFile(filepath.Join(fixture.root, "logs", run.ID+".log"), []byte(output), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.logs.BuildIndex(run.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.store.PutStepResult(ctx, model.StepResult{RunID: run.ID, Burn: "release-build-variants", Step: "release-build-variants", Status: model.StepPassed, DeclaredSize: "M"}); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.store.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
				t.Fatal(err)
			}
			value, err := control.CallTool(ctx, api.Actor{Identity: "agent@host"}, "run_logs", arguments)
			if err != nil {
				t.Fatal(err)
			}
			got := value.(LogResponse)
			if got.RunID != run.ID || got.Output != output || got.TotalLines != 1 || got.MatchedLines != 1 || got.ReturnedLines != 1 {
				t.Fatalf("completed run_logs = %#v", got)
			}
		})
	}
}

func TestRunToolsRejectInvalidIDPrefixesAsInputErrors(t *testing.T) {
	fixture := newControlFixture(t)
	for _, id := range []string{"abc123", "abc12345678", "abc12345678%", "abc12345678_", "../../etc/passwd", strings.Repeat("a", 33)} {
		for _, tool := range []string{"run_get", "run_logs"} {
			args := map[string]string{"id": id}
			if tool == "run_logs" {
				args["burn"], args["step"] = "test", "unit"
			}
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			_, err = fixture.api(t).CallTool(context.Background(), api.Actor{Identity: "agent@host"}, tool, raw)
			if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("%s(%q) = %v, want invalid input", tool, id, err)
			}
		}
	}
}
