package service

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
)

func TestTagSkippedPublicationTimestampsNeedNoForce(t *testing.T) {
	for _, kind := range []string{"skipped", "skipped with synthesized start", "pending", "earlier failed publication", "earlier publication attempt"} {
		for _, tool := range []string{"run_rerun", "run_retry"} {
			t.Run(kind+"/"+tool, func(t *testing.T) {
				at := time.Now().UTC()
				publish := skippedPublisher()
				publish.StartedAt, publish.FinishedAt = &at, &at
				publish.Termination = &model.StepDiagnostic{Unavailable: "not-executed", ObservedAt: &at}
				steps := []model.StepResult{stepResult(model.StepFailed)}
				if kind != "pending" {
					steps = append(steps, publish)
				}
				result := observedTagResult(JobResult{Status: model.RunFailed, Phase: "infrastructure", Error: model.NodeRebootReasonPrefix + "node worker restarted", Steps: steps})
				if kind != "pending" {
					result.TerminalObservation.Steps[1].Started = false
				}
				if kind == "skipped with synthesized start" {
					result.TerminalObservation.Steps[1].Started = true
				}
				if kind == "earlier publication attempt" {
					result.TerminalObservation.Nodes++
					result.TerminalObservation.Steps = append(result.TerminalObservation.Steps, model.RunObservedStep{Stage: "publish", Step: "push", Status: model.StepFailed, Started: true})
				}
				// A declared step with no execution node is pending in the reviewed plan.
				h := newRerunHarness(t, result, greenResult)
				h.fixture.jobs.plan = tagReplayPlan()
				failed := admitRerunTag(t, h)
				ensureTagJournal(t, h, failed)
				if kind == "skipped with synthesized start" {
					if err := h.fixture.logs.AppendStepProgress(failed.ID, runprogress.Event{Version: 1, Stage: "publish", Step: "push", Ordinal: 1, DeclaredSize: "M", Status: runprogress.StepRunning, StartedAt: &at}); err != nil {
						t.Fatal(err)
					}
				}
				if kind != "pending" {
					if err := h.fixture.logs.AppendStepProgress(failed.ID, runprogress.Event{Version: 1, Stage: "publish", Step: "push", Ordinal: 1, DeclaredSize: "M", Status: runprogress.StepSkipped, StartedAt: &at, FinishedAt: &at}); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "earlier failed publication" {
					if err := h.fixture.logs.AppendStepProgress(failed.ID, runprogress.Event{Version: 1, Stage: "publish", Step: "push", Ordinal: 1, DeclaredSize: "M", Status: runprogress.StepFailed, ExitCode: 1, StartedAt: &at, FinishedAt: &at}); err != nil {
						t.Fatal(err)
					}
				}
				_, err := h.call(rerunAdmin, tool, map[string]any{"id": failed.ID})
				if strings.HasPrefix(kind, "earlier") {
					if err == nil {
						t.Fatal("earlier publication execution lost force gate")
					}
				} else if err != nil {
					t.Fatalf("%s required force before publication: %v", tool, err)
				}
			})
		}
	}
}

func TestNodeRebootDisplayKeepsNodeTimeAndRetryLink(t *testing.T) {
	run := model.Run{Status: model.RunFailed, Phase: "infrastructure", Error: model.NodeRebootReasonPrefix + "node worker restarted after 2026-11-11T21:00:00Z; new boot first observed at 2026-11-11T21:01:00Z"}
	shown := presentationRun(run)
	if !strings.Contains(shown.DisplayReason, "node worker restarted") || !strings.Contains(shown.DisplayReason, "2026-11-11T21:01:00Z") {
		t.Fatal(shown.DisplayReason)
	}
}
