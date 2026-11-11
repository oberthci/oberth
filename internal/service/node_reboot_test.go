package service

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
)

func TestNodeRebootBranchRetriesWithinBudget(t *testing.T) {
	now := time.Now()
	f := newRecoveryFixtureWithOptions(t, store.Options{Now: func() time.Time { return now }})
	run := f.enqueueAndClaim(t, "feature/reboot", strings.Repeat("b", 40))
	for attempt := 0; attempt <= model.DefaultInfrastructureAutoRetryBudget; attempt++ {
		name, err := deterministicWorkflowName(f.repo.Name, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		result := evictionResult(name, time.Now().UTC())
		result.Error = model.NodeRebootReasonPrefix + "node worker restarted after 2026-11-11T21:00:00Z; new boot first observed at 2026-11-11T21:01:00Z"
		result.TerminalObservation.Steps[0].Evicted = false
		result.TerminalObservation.Steps[0].NodeReboot = true
		result.Steps[0].ExitCode = 255
		jobs := &rcaCreateFailure{}
		jobs.waitResult = result
		if err = f.scheduler(t, jobs).executeAndCleanup(t.Context(), run); err != nil {
			t.Fatal(err)
		}
		failed, err := f.store.Run(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		hint := runRetryHint(failed, nil)
		if hint == nil || hint.Class != model.RetryInfrastructure {
			t.Fatalf("reboot retry hint: %+v", hint)
		}
		if failed.Status != model.RunFailed || failed.Phase != "infrastructure" || failed.FailedStep != "" {
			t.Fatalf("verdict: %+v", failed)
		}
		if attempt == model.DefaultInfrastructureAutoRetryBudget {
			if failed.AutoRetriedBy != "" {
				t.Fatal("reboot exceeded retry budget")
			}
			break
		}
		if failed.AutoRetriedBy == "" {
			t.Fatalf("reboot not retried: %+v", failed)
		}
		now = now.Add(5 * time.Second)
		run, err = f.store.ClaimNextRun(t.Context())
		if err != nil || run.ID != failed.AutoRetriedBy {
			t.Fatalf("retry queue: %+v %v", run, err)
		}
	}
}

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
	for _, retried := range []bool{false, true} {
		if retried {
			run.AutoRetriedBy = strings.Repeat("a", 32)
		}
		shown := presentationRun(run)
		if !strings.Contains(shown.DisplayReason, "node worker restarted") || !strings.Contains(shown.DisplayReason, "2026-11-11T21:01:00Z") || strings.Contains(shown.DisplayReason, "…") || (retried && !strings.Contains(shown.DisplayReason, "retried as")) {
			t.Fatalf("reason clipped: %s", shown.DisplayReason)
		}
	}
}
