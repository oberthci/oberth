package service

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// startupDeadlineResult is the engine verdict for a step whose Pod deadline
// expired during start-up (#1499): main exited 0 before the Pod was killed.
func startupDeadlineResult(runID, name string, at time.Time) JobResult {
	start := at.Add(-39 * time.Second)
	deadline := int64(300)
	termination := &model.StepDiagnostic{Available: true, ObservedAt: &at, Container: &model.StepTermination{
		RunID: runID, Namespace: "argo", WorkflowName: name, WorkflowUID: "11111111-1111-4111-8111-111111111111", NodeID: name + "-1", PodName: name + "-1",
		PodUID: "22222222-2222-4222-8222-222222222222", ContainerName: "main", ContainerID: "containerd://" + strings.Repeat("b", 64),
		Reason: "Completed", PodReason: "DeadlineExceeded", StartedAt: start, FinishedAt: at, ActiveDeadlineSeconds: &deadline,
	}}
	return JobResult{Status: model.RunFailed, Phase: "infrastructure", Error: model.StartupDeadlineReasonPrefix + "step ci/fmt exited 0, but its Pod deadline of 300s expired outside the step command (Pod start-up or result upload).",
		Steps:               []model.StepResult{{RunID: runID, Stage: "ci", Step: "fmt", Status: model.StepFailed, DeclaredSize: "M", StartedAt: &start, FinishedAt: &at, Termination: termination}},
		TerminalObservation: &model.RunTerminalObservation{Version: 1, WorkflowName: name, Nodes: 2, Steps: []model.RunObservedStep{{Stage: "ci", Step: "fmt", Status: model.StepFailed, Started: true, StartupDeadline: true, StartedAt: &start, FinishedAt: &at}}},
	}
}

// #1499: a startup-deadline kill is infrastructure, keeps its own reason
// (never "Execution deadline exceeded"), and is retried automatically within
// the budget (#1524).
func TestStartupDeadlineStepAutomaticallyRetriesWithinBudget(t *testing.T) {
	f := newRecoveryFixture(t)
	run := f.enqueueAndClaim(t, "feature/startup-deadline", strings.Repeat("b", 40))
	for attempt := 0; attempt <= model.DefaultInfrastructureAutoRetryBudget; attempt++ {
		name, err := deterministicWorkflowName(f.repo.Name, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		jobs := &rcaCreateFailure{}
		jobs.waitResult = startupDeadlineResult(run.ID, name, time.Now().UTC())
		if termination := jobs.waitResult.Steps[0].Termination; termination.Validate() != nil {
			t.Fatalf("fixture termination invalid: %v", termination.Validate())
		}
		if err := f.scheduler(t, jobs).executeAndCleanup(t.Context(), run); err != nil {
			t.Fatal(err)
		}
		failed, err := f.store.Run(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if failed.Status != model.RunFailed || failed.Phase != "infrastructure" || failed.FailedStep != "" || model.IsExecutionDeadlineReason(failed.Error) || !model.IsStartupDeadlineReason(failed.Error) {
			t.Fatalf("startup deadline reported as a step deadline: %+v", failed)
		}
		if attempt == model.DefaultInfrastructureAutoRetryBudget {
			if failed.AutoRetriedBy != "" || failed.AutoRetryOf == "" {
				t.Fatalf("retried past the budget: %+v", failed)
			}
			break
		}
		if failed.AutoRetriedBy == "" {
			t.Fatal("startup deadline not retried")
		}
		run, err = f.store.ClaimNextRun(t.Context())
		if err != nil || run.ID != failed.AutoRetriedBy {
			t.Fatalf("retry not queued: %+v %v", run, err)
		}
	}
}

// A genuine step deadline (no startup evidence) is still never retried.
func TestStepDeadlineWithoutStartupEvidenceIsNotRetried(t *testing.T) {
	at := time.Now().UTC()
	result := startupDeadlineResult(strings.Repeat("a", 32), "workflow", at)
	result.TerminalObservation.Steps[0].StartupDeadline = false
	run := model.Run{ID: strings.Repeat("a", 32), RefKind: model.RefBranch, Trigger: "branch", WorkflowName: "workflow"}
	if (&Scheduler{}).infrastructureAutoRetryEligible(run, result) {
		t.Fatal("step failure without startup evidence retried")
	}
}
