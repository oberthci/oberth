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
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
)

func TestPreStepAutoRetryUsesAllProgressAndExecutionEvidence(t *testing.T) {
	for _, kind := range []string{"empty", "missing", "queued", "started", "earlier-start-then-queued", "torn", "result-started", "observation-started", "earlier-workflow-attempt", "release", "tag", "trigger-tag", "trigger-release", "promotion", "plan", "apply"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			logs, err := runlog.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			scheduler := &Scheduler{logs: logs}
			run := model.Run{ID: strings.Repeat("a", 32), RefKind: model.RefBranch, Trigger: "branch"}
			result := JobResult{Status: model.RunFailed, Phase: "infrastructure"}
			now := time.Now()
			if kind == "empty" {
				if err := logs.InitializeStepProgress(run.ID); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "queued" || kind == "started" || kind == "earlier-start-then-queued" {
				event := runprogress.Event{Version: 1, Burn: "ci", Step: "unit", Ordinal: 0, DeclaredSize: "M", Status: runprogress.StepQueued}
				if kind != "queued" {
					event.Status = runprogress.StepRunning
					event.StartedAt = &now
				}
				if err := logs.AppendStepProgress(run.ID, event); err != nil {
					t.Fatal(err)
				}
				if kind == "earlier-start-then-queued" {
					event.Status = runprogress.StepQueued
					event.StartedAt = nil
					if err := logs.AppendStepProgress(run.ID, event); err != nil {
						t.Fatal(err)
					}
				}
			}
			if kind == "torn" {
				if err := os.WriteFile(filepath.Join(root, run.ID+".progress.jsonl"), []byte(`{"version":1`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "result-started":
				result.Steps = []model.StepResult{{Status: model.StepPassed, StartedAt: &now}}
			case "observation-started":
				result.TerminalObservation = &model.RunTerminalObservation{Steps: []model.RunObservedStep{{Started: true}}}
			case "earlier-workflow-attempt":
				result.ObservedStepStart = true
			case "release":
				run.Release = true
			case "tag":
				run.RefKind = model.RefTag
			case "trigger-tag", "trigger-release":
				run.Trigger = strings.TrimPrefix(kind, "trigger-")
			case "promotion", "plan", "apply":
				run.Trigger = kind
			}
			want := kind == "empty" || kind == "queued"
			if got := scheduler.preStepAutoRetry(run, result); got != want {
				t.Fatal("pre-step retry eligibility", kind, got, want)
			}
		})
	}
}

func TestAutomaticRetryPresentationPreservesRawFailureAndShowsAmber(t *testing.T) {
	raw := model.Run{ID: "old", Status: model.RunFailed, Phase: "infrastructure", Error: "executor identity failed", AutoRetriedBy: "new-retry"}
	view := presentationRun(raw)
	if view.Status != model.RunFailed || view.Error != raw.Error || view.DisplayStatus != "infra-retried" || !strings.Contains(view.DisplayReason, "infra error, retried") {
		t.Fatal("lost retry cause or raw status", view)
	}
	if got := presentationRun(model.Run{Status: model.RunInterrupted, SupersededBy: "new"}); got.Status != model.RunInterrupted || got.DisplayStatus != "superseded" {
		t.Fatal("superseded was renamed on wire or still cancelled", got)
	}
}

type rcaCreateFailure struct {
	recoveryJobs
	createErr   error
	createCalls int
}

func (j *rcaCreateFailure) CreateCI(context.Context, JobRequest) error {
	j.createCalls++
	return j.createErr
}

func TestAutomaticRetryRequiresFailedCreateCleanupAndKeepsStartedSteps(t *testing.T) {
	for _, kind := range []string{"create", "create-delete-fails", "started-before-identity", "pre-step-deadline"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			run := fixture.enqueueAndClaim(t, "feature/"+kind, strings.Repeat("b", 40))
			jobs := &rcaCreateFailure{createErr: errors.New("API create failed")}
			if kind == "create-delete-fails" {
				jobs.deleteErr = errors.New("API delete unavailable")
			}
			if kind == "pre-step-deadline" {
				jobs.createErr = nil
				jobs.waitResult = JobResult{Status: model.RunFailed, Phase: "job", Error: model.ExecutionDeadlineMessage}
			}
			if kind == "started-before-identity" {
				jobs.createErr = nil
				jobs.waitErr = &identityObservationFailure{cause: context.DeadlineExceeded}
				jobs.terminalErr = ErrJobNotTerminal
				jobs.waitResult = JobResult{Steps: []model.StepResult{{Burn: "ci", Step: "unit", Status: model.StepPassed, DeclaredSize: "M"}}}
			}
			err := fixture.scheduler(t, jobs).executeAndCleanup(t.Context(), run)
			current, lookupErr := fixture.store.Run(t.Context(), run.ID)
			if lookupErr != nil {
				t.Fatal(lookupErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind == "create" || kind == "create-delete-fails" {
				if current.Status != model.RunFailed || current.AutoRetriedBy != "" || len(jobs.deleted) != 0 {
					t.Fatal("unproven create error retried or deleted a possibly foreign name", current, jobs.deleted)
				}
			}
			if kind == "pre-step-deadline" && (current.AutoRetriedBy != "" || current.Phase != "job") {
				t.Fatal("post-create deadline retried or raw phase rewritten", current)
			}
			if kind == "started-before-identity" && current.AutoRetriedBy != "" {
				t.Fatal("wait error erased started-step evidence", current)
			}
		})
	}
}
