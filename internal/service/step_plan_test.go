package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
)

// Stored native outcomes flow through run detail and status without plan merging.
func TestDeclaredRetryOutcomeInRunDetailAndStatus(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "passed", true: "failed"}[failed], func(t *testing.T) {
			at := time.Unix(1000, 0).UTC()
			result := JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{
				{NodeID: "retry.check", Stage: "retry", Step: "check", Status: model.StepPassed, StartedAt: &at, FinishedAt: &at},
			}}
			if failed {
				result.Status, result.FailedStage, result.FailedStep = model.RunFailed, "retry", "check"
				result.Steps[0].Status, result.Steps[0].ExitCode = model.StepFailed, 42
			}
			fixture := newControlFixture(t, result)
			fixture.jobs.plan = []runprogress.PlannedStep{{Stage: "retry", Step: "check", Ordinal: 26}}
			ctx := context.Background()
			run, err := fixture.scheduler.recordCIForTest(ctx, CIRequest{
				EventID: "retry", Repository: fixture.repo, Branch: "feature/retry",
				SHA: strings.Repeat("a", 40), Actor: "agent@host",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.scheduler.ProcessNext(ctx); err != nil {
				t.Fatal(err)
			}
			// Seed a retained historical range for the read fixture.
			if err := fixture.scheduler.seedHistoricalSteps(ctx, fixture.store, run.ID, result.Steps, runlog.Index{Ranges: []runlog.Range{
				{Stage: "retry", Step: "check", Start: 10, End: 100},
			}}); err != nil {
				t.Fatal(err)
			}
			control := fixture.api(t)
			steps := runDetailSteps(t, control, run.ID)
			if len(steps) != 1 || steps[0].Ordinal != 0 || steps[0].Status != result.Steps[0].Status ||
				steps[0].ExitCode != result.Steps[0].ExitCode ||
				steps[0].StartedAt == nil || !steps[0].StartedAt.Equal(at) || steps[0].FinishedAt == nil || !steps[0].FinishedAt.Equal(at) {
				t.Fatalf("declared result: %+v", steps)
			}
			status, err := control.status(ctx, fixture.repo.Name, run.SHA, "")
			if err != nil || status.Stages["retry"] != string(result.Steps[0].Status) || len(status.Stages) != 1 {
				t.Fatalf("status: %+v: %v", status, err)
			}
		})
	}
}

// unwrapPathError lets a test assert "no plan file" through the wrapped error
// StepPlan returns.
func unwrapPathError(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr
	}
	return err
}

func stepStates(steps []model.StepResult) string {
	parts := make([]string, 0, len(steps))
	for _, step := range steps {
		parts = append(parts, step.Stage+"/"+step.Step+"="+string(step.Status))
	}
	return strings.Join(parts, " ")
}

func runDetailSteps(t *testing.T, control *API, runID string) []model.StepResult {
	t.Helper()
	value, err := control.Run(context.Background(), api.Actor{Identity: "dashboard@viewer"}, runID)
	if err != nil {
		t.Fatal(err)
	}
	return value.(RunDetailResponse).Steps
}

// Stored outcomes remain readable without a declaration plan.
func TestRunWithoutAPlanKeepsItsExistingBehaviour(t *testing.T) {
	fixture := newControlFixture(t, JobResult{
		Status: model.RunPassed, Phase: "passed",
		Steps: []model.StepResult{
			{NodeID: "test.unit", Stage: "test", Step: "unit", Status: model.StepPassed},
			{NodeID: "test.race", Ordinal: 1, Stage: "test", Step: "race", Status: model.StepPassed},
		},
	})
	fixture.jobs.plan = nil
	ctx := context.Background()
	enqueued, err := fixture.scheduler.recordCIForTest(ctx, CIRequest{
		EventID: "receive-planless", Repository: fixture.repo,
		Branch: "feature/planless", SHA: "5555555555555555555555555555555555555555", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "logs", enqueued.ID+".plan.json")); !os.IsNotExist(unwrapPathError(err)) {
		t.Fatalf("a plan was written for an engine that reported none: %v", err)
	}
	steps := runDetailSteps(t, fixture.api(t), enqueued.ID)
	if got := stepStates(steps); got != "test/unit=passed test/race=passed" {
		t.Fatalf("step list = %s", got)
	}
}

// Native outcomes do not require a separate declaration plan.
func TestNativeStepHistoryDoesNotNeedADeclarationPlan(t *testing.T) {
	fixture := newControlFixture(t, JobResult{
		Status: model.RunPassed, Phase: "passed",
		Steps: []model.StepResult{{NodeID: "test.unit", Stage: "test", Step: "unit", Status: model.StepPassed}},
	})
	ctx := context.Background()
	enqueued, err := fixture.scheduler.recordCIForTest(ctx, CIRequest{
		EventID: "receive-unplannable", Repository: fixture.repo,
		Branch: "feature/unplannable", SHA: "6666666666666666666666666666666666666666", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := fixture.store.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunPassed {
		t.Fatalf("run status = %q; an unenumerable plan failed the build", run.Status)
	}
	body, err := fixture.logs.Tail(enqueued.ID, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "step plan unavailable") {
		t.Fatalf("the run log contains a retired plan diagnostic: %q", body)
	}
}

// TestWireBurnsDoesNotDependOnStepOrder guards the burns projection against the
// order its input happens to arrive in. A burn with a passed step and a step
// that was never reached is not a passed burn.
func TestWireStagesDoesNotDependOnStepOrder(t *testing.T) {
	forward := []model.StepResult{
		{NodeID: "setup.one", Stage: "setup", Step: "one", Status: model.StepPassed},
		{NodeID: "setup.two", Stage: "setup", Step: "two", Status: model.StepPending},
	}
	reversed := []model.StepResult{forward[1], forward[0]}
	for name, steps := range map[string][]model.StepResult{"forward": forward, "reversed": reversed} {
		stages, _ := wireStages(steps, model.Run{})
		if stages["setup"] != "pending" {
			t.Errorf("%s: burn = %q, want pending", name, stages["setup"])
		}
	}
	stages, _ := wireStages([]model.StepResult{
		{NodeID: "b.skipped", Stage: "b", Step: "skipped", Status: model.StepSkipped},
		{NodeID: "b.passed", Stage: "b", Step: "passed", Status: model.StepPassed},
	}, model.Run{})
	if stages["b"] != "passed" {
		t.Errorf("a burn of one passed and one skipped step = %q, want passed", stages["b"])
	}
	stages, _ = wireStages([]model.StepResult{
		{NodeID: "c.pending", Stage: "c", Step: "pending", Status: model.StepPending},
		{NodeID: "c.failed", Stage: "c", Step: "failed", Status: model.StepFailed},
	}, model.Run{})
	if stages["c"] != "failed" {
		t.Errorf("a burn containing a failure = %q, want failed", stages["c"])
	}
}

// TestMergeStepsCarriesPlannedSecrets checks that the per-step secret flag
// survives every source that can advance a declared row, and never appears on
// a row the plan did not mark: only the plan knows which steps run a
// credential chain.
