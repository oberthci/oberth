package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
)

func tagReplayPlan() []runprogress.PlannedStep {
	return []runprogress.PlannedStep{{Burn: "test", Step: "unit", Ordinal: 0}, {Burn: "publish", Step: "push", Ordinal: 1, Secrets: true}}
}
func skippedPublisher() model.StepResult {
	return model.StepResult{Burn: "publish", Step: "push", Ordinal: 1, Status: model.StepSkipped, DeclaredSize: "M"}
}

func TestTagRunRerunBeforeCredentialedStepsNeedsNoForce(t *testing.T) {
	harness := newRerunHarness(t, JobResult{Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit", Steps: []model.StepResult{stepResult(model.StepFailed), skippedPublisher()}}, greenResult)
	harness.fixture.jobs.plan = tagReplayPlan()
	harness.fixture.jobs.transcripts = []string{"[test/unit] failed before publish\n"}
	failed := admitRerunTag(t, harness)
	if _, err := harness.call(rerunUplink, "run_rerun", map[string]any{"id": failed.ID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("tag lost admin gate: %v", err)
	}
	response := harness.rerun(t, rerunAdmin, map[string]any{"id": failed.ID, "reason": "test failed before publish"})
	if response.Warning != "" || response.Mode != "rerun" {
		t.Fatalf("safe tag needs force/warning: %+v", response)
	}
	queued := harness.run(t, response.RunID)
	if !queued.Release || !queued.Credentialed || queued.SHA != failed.SHA || queued.TestedSHA != failed.TestedSHA || queued.RefKind != model.RefTag {
		t.Fatalf("lost admission: %+v", queued)
	}
	action, err := harness.fixture.store.LatestAuditActionByType(t.Context(), "run.rerun", "run")
	if err != nil || !strings.Contains(action.Details, `"force":false`) {
		t.Fatalf("safe rerun audit: %+v %v", action, err)
	}
	harness.process(t)
	if harness.run(t, queued.ID).Status != model.RunPassed || harness.git.tagAttempts() != 1 {
		t.Fatal("safe tag rerun did not publish once")
	}
	if _, err := harness.call(rerunAdmin, "run_rerun", map[string]any{"id": failed.ID, "force": true}); err == nil {
		t.Fatal("published tag was rerun")
	}
}

func TestTagRunReplayRequiresForceAfterSecretsStarted(t *testing.T) {
	for _, kind := range []string{"running journal", "passed", "failed", "skipped after start", "failed step without result", "legacy attempt", "started then queued"} {
		t.Run(kind, func(t *testing.T) {
			result := infraFailure
			if kind == "failed step without result" {
				result = JobResult{Status: model.RunFailed, Phase: "test", FailedBurn: "publish", FailedStep: "push"}
			}
			harness := newRerunHarness(t, result)
			harness.fixture.jobs.plan = tagReplayPlan()
			failed := admitRerunTag(t, harness)
			now := time.Now()
			switch kind {
			case "running journal":
				if err := harness.fixture.logs.AppendStepProgress(failed.ID, runprogress.Event{Version: 1, Burn: "publish", Step: "push", Ordinal: 1, DeclaredSize: "M", Status: runprogress.StepRunning, StartedAt: &now}); err != nil {
					t.Fatal(err)
				}
			case "failed step without result": // source's failed-step identity is evidence, too.
			case "started then queued":
				for _, event := range []runprogress.Event{{Version: 1, Burn: "publish", Step: "push", Ordinal: 1, DeclaredSize: "M", Status: runprogress.StepRunning, StartedAt: &now}, {Version: 1, Burn: "publish", Step: "push", Ordinal: 1, DeclaredSize: "M", Status: runprogress.StepQueued}} {
					if err := harness.fixture.logs.AppendStepProgress(failed.ID, event); err != nil {
						t.Fatal(err)
					}
				}

			default:
				result := skippedPublisher()
				switch kind {
				case "passed":
					result.Status = model.StepPassed
				case "failed":
					result.Status = model.StepFailed
				case "skipped after start":
					result.StartedAt = &now
				case "legacy attempt":
					result.Burn = "publish(0)"
					result.Status = model.StepPassed
				}
				result.RunID = failed.ID
				if _, err := harness.fixture.store.PutStepResult(t.Context(), result); err != nil {
					t.Fatal(err)
				}
			}
			for _, tool := range []string{"run_rerun", "run_retry"} {
				if _, err := harness.call(rerunAdmin, tool, map[string]any{"id": failed.ID}); err == nil {
					t.Fatalf("%s ignored started credentialed work", tool)
				}
			}
			forced := harness.rerun(t, rerunAdmin, map[string]any{"id": failed.ID, "force": true})
			if forced.Warning == "" {
				t.Fatal("forced release warning lost")
			}
		})
	}
}

func TestTagRunRetryInfrastructureKeepsReleaseAdmission(t *testing.T) {
	for _, phase := range []string{"infrastructure", "job"} {
		t.Run(phase, func(t *testing.T) {
			harness := newRerunHarness(t, JobResult{Status: model.RunFailed, Phase: phase, Error: "executor identity unavailable"}, greenResult)
			harness.fixture.jobs.plan = tagReplayPlan()
			failed := admitRerunTag(t, harness)
			if _, err := harness.call(rerunUplink, "run_retry", map[string]any{"id": failed.ID}); !errors.Is(err, ErrForbidden) {
				t.Fatalf("tag retry lost admin gate: %v", err)
			}
			value, err := harness.call(rerunAdmin, "run_retry", map[string]any{"id": failed.ID[:8]})
			if err != nil {
				t.Fatal(err)
			}
			retried := value.(model.Run)
			if !retried.Release || !retried.Credentialed || retried.SHA != failed.SHA || retried.TestedSHA != failed.TestedSHA || retried.Trigger != failed.Trigger || retried.Ref != failed.Ref || retried.Actor != rerunAdmin.Identity {
				t.Fatalf("retry changed tag admission: %+v", retried)
			}
			if _, err := harness.call(rerunAdmin, "run_retry", map[string]any{"id": failed.ID}); !errors.Is(err, store.ErrInvalidState) {
				t.Fatalf("retry beside active/newer run: %v", err)
			}
			harness.process(t)
			if harness.run(t, retried.ID).Status != model.RunPassed || harness.git.tagAttempts() != 1 {
				t.Fatal("tag retry did not execute release/publish once")
			}
			if _, err := harness.call(rerunAdmin, "run_retry", map[string]any{"id": failed.ID}); !errors.Is(err, store.ErrInvalidState) {
				t.Fatalf("old tag retry accepted: %v", err)
			}
		})
	}
}

func TestTagRunRerunUnknownEvidenceStillNeedsForce(t *testing.T) {
	for _, kind := range []string{"missing plan", "corrupt plan", "corrupt progress", "torn progress"} {
		t.Run(kind, func(t *testing.T) {
			harness := newRerunHarness(t, infraFailure)
			harness.fixture.jobs.plan = tagReplayPlan()
			failed := admitRerunTag(t, harness)
			switch kind {
			case "missing plan":
				if err := os.Remove(filepath.Join(harness.fixture.root, "logs", failed.ID+".plan.json")); err != nil {
					t.Fatal(err)
				}
			case "corrupt plan":
				if err := os.WriteFile(filepath.Join(harness.fixture.root, "logs", failed.ID+".plan.json"), []byte("bad\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "torn progress":
				if err := os.WriteFile(filepath.Join(harness.fixture.root, "logs", failed.ID+".progress.jsonl"), []byte("{\"version\":"), 0600); err != nil {
					t.Fatal(err)
				}
			case "corrupt progress":
				if err := os.WriteFile(filepath.Join(harness.fixture.root, "logs", failed.ID+".progress.jsonl"), []byte("bad\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := harness.call(rerunAdmin, "run_rerun", map[string]any{"id": failed.ID}); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("unknown evidence allowed: %v", err)
			}
			harness.rerun(t, rerunAdmin, map[string]any{"id": failed.ID, "force": true})
		})
	}
}

func TestTagRunRetryBeforeWorkflowIntentNeedsNoPlan(t *testing.T) {
	harness := newRerunHarness(t)
	queued, err := harness.fixture.store.EnqueueRun(context.Background(), model.RunSpec{RepoID: harness.fixture.repo.ID, RefKind: model.RefTag, Ref: rerunTag, SHA: rerunTagObject, TestedSHA: rerunTagCommit, Release: true, Credentialed: true, Trigger: "tag", Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.fixture.store.FinishRun(t.Context(), queued.ID, model.RunResult{Status: model.RunFailed, Phase: "infrastructure", Error: "checkout unavailable"}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.call(rerunAdmin, "run_retry", map[string]any{"id": queued.ID}); err != nil {
		t.Fatal(err)
	}
}

func TestTagRunPublicationFailureStillNeedsForce(t *testing.T) {
	harness := newRerunHarness(t, greenResult)
	harness.fixture.jobs.plan = tagReplayPlan()
	harness.git.failTags = 1
	failed := admitRerunTag(t, harness)
	if _, err := harness.call(rerunAdmin, "run_rerun", map[string]any{"id": failed.ID}); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "force") {
		t.Fatalf("publication rerun lost confirmation: %v", err)
	}
	if _, err := harness.call(rerunAdmin, "run_retry", map[string]any{"id": failed.ID}); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("publication was re-executed by run_retry: %v", err)
	}
	response := harness.rerun(t, rerunAdmin, map[string]any{"id": failed.ID, "force": true})
	if response.Mode != "publish_retry" || len(harness.fixture.jobs.createdReleases) != 1 || harness.git.tagAttempts() != 2 {
		t.Fatalf("failed publication re-executed steps: %+v", response)
	}
}
