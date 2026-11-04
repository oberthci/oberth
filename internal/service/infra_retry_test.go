package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

func TestInfrastructureRetryTrustedPolicyDeadlineKeepsQueuePosition(t *testing.T) {
	f := newControlFixture(t)
	f.scheduler.git = &policyTestGit{controlGit: f.git, baselineErr: fmt.Errorf("%w: refresh upstream trusted test policy: %w", gitcache.ErrPolicyUpstreamUnavailable, context.DeadlineExceeded)}
	f.scheduler.policyWaitTimeout = time.Millisecond
	f.scheduler.capacityWait = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	source, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "policy-retry", Repository: f.repo, Branch: "feature/policy", SHA: strings.Repeat("a", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "other", Repository: f.repo, Branch: "feature/later", SHA: strings.Repeat("b", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	failed, err := f.store.Run(t.Context(), source.ID)
	if err != nil || failed.AutoRetriedBy == "" || !strings.Contains(failed.Error, "context deadline exceeded") {
		t.Fatalf("deadline did not retry: %+v %v", failed, err)
	}
	retry, err := f.store.Run(t.Context(), failed.AutoRetriedBy)
	reason := "automatic retry of " + source.ID + " after infrastructure failure: " + failed.Error
	if err != nil || retry.AdmissionSequence != source.AdmissionSequence || retry.Priority != source.Priority || retry.Reason != reason || retry.AutoRetryOf != source.ID {
		t.Fatalf("retry identity/order/reason: %+v %v", retry, err)
	}
	action, err := f.store.LatestAuditActionByType(t.Context(), "run.retry.auto", "run")
	var details map[string]any
	if err != nil || json.Unmarshal([]byte(action.Details), &details) != nil || details["reason"] != reason {
		t.Fatalf("retry audit: %+v %v", action, err)
	}
	if err = f.scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	exhausted, err := f.store.Run(t.Context(), retry.ID)
	if err != nil || exhausted.Status != model.RunFailed || exhausted.AutoRetriedBy != "" {
		t.Fatalf("retry was not next or exceeded cap: %+v %v", exhausted, err)
	}
	untouched, err := f.store.Run(t.Context(), other.ID)
	if err != nil || untouched.Status != model.RunQueued {
		t.Fatalf("retry went to back of queue: %+v %v", untouched, err)
	}
	runs, err := f.store.ListRecentRuns(t.Context(), model.RunListFilter{RepoID: f.repo.ID, Limit: 10})
	if err != nil || len(runs) != 3 {
		t.Fatalf("attempt count: %d %v", len(runs), err)
	}
}

func TestInfrastructureRetryNeverReplaysTestFailure(t *testing.T) {
	for _, phase := range []string{"test", "job", "infrastructure"} {
		t.Run(phase, func(t *testing.T) {
			result := JobResult{Status: model.RunFailed, Phase: phase, Error: "assertion failed", Steps: []model.StepResult{{Stage: "ci", Step: "unit", DeclaredSize: "M", Status: model.StepFailed}}}
			if phase == "test" {
				result.FailedStage, result.FailedStep = "ci", "unit"
			}
			f := newControlFixture(t, result)
			source, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "test-failure", Repository: f.repo, Branch: "feature/unit", SHA: strings.Repeat("c", 40), Actor: "pusher"})
			if err != nil {
				t.Fatal(err)
			}
			if err = f.scheduler.ProcessNext(t.Context()); err != nil {
				t.Fatal(err)
			}
			failed, err := f.store.Run(t.Context(), source.ID)
			if err != nil || failed.Status != model.RunFailed || failed.AutoRetriedBy != "" {
				t.Fatalf("test failure retried: %+v %v", failed, err)
			}
			runs, err := f.store.ListRecentRuns(t.Context(), model.RunListFilter{RepoID: f.repo.ID, Limit: 10})
			if err != nil || len(runs) != 1 {
				t.Fatalf("unexpected attempts: %d %v", len(runs), err)
			}
		})
	}
}

func TestInfrastructureRetryWorkflowFailureAndLostExecutor(t *testing.T) {
	for _, kind := range []string{"workflow", "lost-executor", "disabled", "reconciled"} {
		t.Run(kind, func(t *testing.T) {
			result := JobResult{Status: model.RunFailed, Phase: "job", Error: "workflow controller failed", Steps: []model.StepResult{{Stage: "ci", Step: "setup", DeclaredSize: "M", Status: model.StepPassed}}}
			f := newControlFixture(t, result)
			if kind == "disabled" {
				scheduler, err := NewScheduler(SchedulerConfig{DisableInfrastructureAutoRetry: true, Store: f.store, Git: f.git, Logs: f.logs, Jobs: f.jobs, Auditor: f.store, WorkspaceRoot: f.scheduler.workspaceRoot})
				if err != nil {
					t.Fatal(err)
				}
				f.scheduler = scheduler
			}
			if kind == "lost-executor" {
				f.jobs.waitErr = &identityObservationFailure{cause: context.DeadlineExceeded}
			}
			source, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: kind, Repository: f.repo, Branch: "feature/" + kind, SHA: strings.Repeat("d", 40), Actor: "pusher"})
			if err != nil {
				t.Fatal(err)
			}
			if kind == "reconciled" {
				run, e := f.store.ClaimNextRun(t.Context())
				if e != nil {
					t.Fatal(e)
				}
				if e = f.logs.InitializeStepProgress(run.ID); e != nil {
					t.Fatal(e)
				}
				err = f.scheduler.finalizeReconciledRun(t.Context(), run, result, f.scheduler.requireMutation)
			} else {
				err = f.scheduler.ProcessNext(t.Context())
			}
			if err != nil {
				t.Fatal(err)
			}
			failed, err := f.store.Run(t.Context(), source.ID)
			if err != nil || (failed.AutoRetriedBy != "") != (kind != "disabled") {
				t.Fatalf("automatic eligibility %s: %+v %v", kind, failed, err)
			}
			if kind == "lost-executor" && len(f.jobs.deleted) != 1 {
				t.Fatal("retry before executor cleanup")
			}
			if failed.AutoRetriedBy != "" {
				retry, e := f.store.Run(t.Context(), failed.AutoRetriedBy)
				if e != nil || !strings.Contains(presentationRun(retry).DisplayReason, "automatic retry of "+source.ID+" after infrastructure failure:") {
					t.Fatalf("retry message missing: %+v %v", retry, e)
				}
			}
		})
	}
}

func TestInfrastructureRetryKeepsPartialFailedStepAttribution(t *testing.T) {
	f := newRecoveryFixture(t)
	run := f.enqueueAndClaim(t, "feature/partial-failure", strings.Repeat("e", 40))
	jobs := &recoveryJobs{waitErr: &identityObservationFailure{cause: context.DeadlineExceeded}, terminalErr: ErrJobNotTerminal, waitResult: JobResult{Status: model.RunFailed, Phase: "test", FailedStage: "ci", FailedStep: "unit"}}
	if err := f.scheduler(t, jobs).executeAndCleanup(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	failed, err := f.store.Run(t.Context(), run.ID)
	if err != nil || failed.AutoRetriedBy != "" || failed.FailedStep != "unit" {
		t.Fatalf("observation error erased test failure: %+v %v", failed, err)
	}
}
