package service

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func evictionResult(name string, at time.Time) JobResult {
	return JobResult{Status: model.RunFailed, Phase: "infrastructure", Error: "Evicted: The node was low on resource: ephemeral-storage.",
		Steps:               []model.StepResult{{Stage: "ci", Step: "install-helm", Status: model.StepFailed, DeclaredSize: "M", StartedAt: &at, FinishedAt: &at}},
		TerminalObservation: &model.RunTerminalObservation{Version: 1, WorkflowName: name, Nodes: 2, Steps: []model.RunObservedStep{{Stage: "ci", Step: "install-helm", Status: model.StepFailed, Started: true, Evicted: true, StartedAt: &at, FinishedAt: &at}}},
	}
}

func TestEvictionRetryRequiresEveryRetainedFailureToMatch(t *testing.T) {
	for _, kind := range []string{"eviction", "reboot", "reboot earlier failure", "reboot mixed observation", "storage limit", "real failure", "earlier failure", "other step", "unknown attempt", "incomplete", "wrong workflow", "empty workflow", "already retried"} {
		t.Run(kind, func(t *testing.T) {
			at := time.Now().UTC()
			result := evictionResult("workflow", at)
			if strings.HasPrefix(kind, "reboot") {
				result.Error = model.NodeRebootReasonPrefix + "node worker restarted"
				result.TerminalObservation.Steps[0].Evicted = false
				result.TerminalObservation.Steps[0].NodeReboot = true
			}
			logs, err := runlog.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			run := model.Run{ID: strings.Repeat("a", 32), RefKind: model.RefBranch, Trigger: "branch", WorkflowName: "workflow"}
			if err := logs.InitializeStepProgress(run.ID); err != nil {
				t.Fatal(err)
			}
			event := runprogress.Event{Version: 1, Stage: "ci", Step: "install-helm", Status: runprogress.StepFailed, DeclaredSize: "M", StartedAt: &at, FinishedAt: &at}
			switch kind {
			case "real failure":
				result.TerminalObservation.Steps[0].Evicted = false
			case "earlier failure", "reboot earlier failure":
				old := at.Add(-time.Minute)
				event.StartedAt, event.FinishedAt = &old, &old
			case "reboot mixed observation":
				result.TerminalObservation.Nodes++
				result.TerminalObservation.Steps = append(result.TerminalObservation.Steps, model.RunObservedStep{Stage: "ci", Step: "real", Status: model.StepFailed, Started: true, StartedAt: &at, FinishedAt: &at})
			case "other step":
				event.Step = "unit"
			case "unknown attempt":
				result.TerminalObservation.Steps[0].StartedAt = nil
			case "incomplete":
				result.TerminalObservation = nil
			case "wrong workflow":
				result.TerminalObservation.WorkflowName = "foreign"
			case "empty workflow":
				run.WorkflowName = ""
			case "storage limit":
				// The retry hint classifies the step's own limit as code (#1524).
				result.Error = "Evicted: Pod ephemeral local storage usage exceeds the total limit of containers 512Mi."
			case "already retried":
				run.AutoRetriedBy = "next"
			}
			if err := logs.AppendStepProgress(run.ID, event); err != nil {
				t.Fatal(err)
			}
			got := (&Scheduler{logs: logs}).infrastructureAutoRetryEligible(run, result)
			if got != (kind == "eviction" || kind == "reboot") {
				t.Fatalf("retry=%v", got)
			}
		})
	}
}

func TestEvictedStepAutomaticallyRetriesWithinBudget(t *testing.T) {
	now := time.Now()
	f := newRecoveryFixtureWithOptions(t, store.Options{Now: func() time.Time { return now }})
	run := f.enqueueAndClaim(t, "feature/eviction", strings.Repeat("b", 40))
	for attempt := 0; attempt <= model.DefaultInfrastructureAutoRetryBudget; attempt++ {
		name, err := deterministicWorkflowName(f.repo.Name, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		jobs := &rcaCreateFailure{}
		jobs.waitResult = evictionResult(name, time.Now().UTC())
		if err := f.scheduler(t, jobs).executeAndCleanup(t.Context(), run); err != nil {
			t.Fatal(err)
		}
		failed, err := f.store.Run(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if failed.Status != model.RunFailed || failed.Phase != "infrastructure" || failed.FailedStep != "" {
			t.Fatalf("lost eviction verdict: %+v", failed)
		}
		steps, err := f.store.StepResults(t.Context(), run.ID)
		if err != nil || len(steps) != 1 || steps[0].Status != model.StepFailed {
			t.Fatalf("lost failed-step evidence: %+v %v", steps, err)
		}
		if attempt == model.DefaultInfrastructureAutoRetryBudget {
			if failed.AutoRetriedBy != "" || failed.AutoRetryOf == "" {
				t.Fatalf("retried past the budget: %+v", failed)
			}
			break
		}
		if failed.AutoRetriedBy == "" {
			t.Fatalf("eviction not retried at attempt %d", attempt)
		}
		now = now.Add(5 * time.Second)
		run, err = f.store.ClaimNextRun(t.Context())
		if err != nil || run.ID != failed.AutoRetriedBy {
			t.Fatalf("retry not queued: %+v %v", run, err)
		}
	}
}

func TestDeterministicAdmissionRefusalsDoNotAutoRetry(t *testing.T) {
	for name, cause := range map[string]error{
		"configuration": &argoworkflow.AdmissionError{Cause: fmt.Errorf("invalid workflow configuration")},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			f := newRecoveryFixtureWithOptions(t, store.Options{Now: func() time.Time { return now }})
			run := f.enqueueAndClaim(t, "feature/"+name, strings.Repeat("b", 40))
			jobs := &rcaCreateFailure{createErr: fmt.Errorf("admission: %w", cause)}
			if err := f.scheduler(t, jobs).executeAndCleanup(t.Context(), run); err != nil {
				t.Fatal(err)
			}
			failed, err := f.store.Run(t.Context(), run.ID)
			if err != nil || failed.AutoRetriedBy != "" || failed.Status != model.RunFailed {
				t.Fatalf("deterministic refusal retried: %+v %v", failed, err)
			}
		})
	}
}

func TestEvictionDisplayReason(t *testing.T) {
	for message, want := range map[string]string{
		"Evicted: Pod ephemeral local storage usage exceeds the total limit of containers 512Mi.": "evicted: ephemeral storage limit 512 MiB; raise the step's ephemeral-storage limit",
		"Evicted: The node was low on resource: memory.":                                          "evicted: node memory pressure",
		"Evicted: The node was low on resource: ephemeral-storage.":                               "evicted: node storage pressure",
		"Evicted": "evicted by kubelet",
	} {
		run := model.Run{Status: model.RunFailed, Phase: "infrastructure", Error: message}
		if got := presentationRun(run).DisplayReason; got != want {
			t.Errorf("%q != %q", got, want)
		}
		run.AutoRetriedBy = strings.Repeat("b", 32)
		if got := presentationRun(run).DisplayReason; !strings.Contains(got, want) || !strings.Contains(got, "retried as") {
			t.Errorf("lost eviction retry cause: %s", got)
		}
	}
}

// Mirrors the controller's pre-create wrapper: false is not proof of a
// deterministic refusal, and the underlying error must remain inspectable.
type preparationRetryFailure struct{ cause error }

func (e *preparationRetryFailure) Error() string             { return e.cause.Error() }
func (e *preparationRetryFailure) Unwrap() error             { return e.cause }
func (*preparationRetryFailure) InfrastructureFailure() bool { return false }
func (*preparationRetryFailure) BeforeWorkflowCreate() bool  { return true }

func TestPreparationRetryOnlyRejectsExplicitDeterministicRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		retry bool
	}{
		{"seed-stream", fmt.Errorf("stream source checkout: temporary read failure"), true},
		{"source-claim-quota", fmt.Errorf("create source claim: quota 403"), true},
		{"source-claim-conflict", fmt.Errorf("create source claim: conflict"), true},
		{"identity-refresh", fmt.Errorf("refresh nonroot identity: transient read"), true},
		{"admission", &argoworkflow.AdmissionError{Cause: fmt.Errorf("invalid workflow")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			f := newRecoveryFixtureWithOptions(t, store.Options{Now: func() time.Time { return now }})
			run := f.enqueueAndClaim(t, "feature/"+tc.name, strings.Repeat("c", 40))
			for attempt := 0; attempt <= model.DefaultInfrastructureAutoRetryBudget; attempt++ {
				jobs := &rcaCreateFailure{createErr: &preparationRetryFailure{cause: tc.cause}}
				if err := f.scheduler(t, jobs).executeAndCleanup(t.Context(), run); err != nil {
					t.Fatal(err)
				}
				failed, err := f.store.Run(t.Context(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if (failed.AutoRetriedBy != "") != (tc.retry && attempt < model.DefaultInfrastructureAutoRetryBudget) {
					t.Fatalf("attempt %d: %+v", attempt, failed)
				}
				if failed.AutoRetriedBy == "" {
					break
				}
				now = now.Add(5 * time.Second)
				run, err = f.store.ClaimNextRun(t.Context())
				if err != nil || run.ID != failed.AutoRetriedBy {
					t.Fatalf("retry: %+v %v", run, err)
				}
			}
		})
	}
}
