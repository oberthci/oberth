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

func TestTerminalDeclaredRowsKeepOrdinalsAndRealOutcomes(t *testing.T) {
	for _, status := range []model.RunStatus{model.RunPassed, model.RunFailed, model.RunInterrupted, model.RunRunning} {
		t.Run(string(status), func(t *testing.T) {
			run := model.Run{ID: "declared", Status: status}
			plan := []runprogress.PlannedStep{
				{Burn: "retry", Step: "check", Ordinal: 26},
				{Burn: "branch", Step: "unused", Ordinal: 30},
			}
			at := time.Unix(1000, 0).UTC()
			result := model.StepResult{RunID: run.ID, Burn: "retry", Step: "check", Ordinal: 0,
				Status: model.StepPassed, StartedAt: &at, FinishedAt: &at, LogStart: 10, LogEnd: 100}
			if status == model.RunFailed {
				result.Status, result.ExitCode = model.StepFailed, 42
			}
			steps := mergeSteps(run, plan, nil, []model.StepResult{result})
			if len(steps) != 2 {
				t.Fatalf("steps: %+v", steps)
			}
			result.Ordinal = 26
			if steps[0] != result {
				t.Fatalf("changed actual outcome: %+v, want %+v", steps[0], result)
			}
			want := model.StepPending
			if status.Terminal() {
				want = model.StepSkipped
			}
			if steps[1].Status != want || steps[1].Ordinal != 30 || steps[1].StartedAt != nil ||
				steps[1].FinishedAt != nil || steps[1].LogStart != 0 || steps[1].LogEnd != 0 {
				t.Fatalf("unreached declaration: %+v, want %s at ordinal 30", steps[1], want)
			}
		})
	}
}

// This regression guard for #1176's first fix checks that a canonical outcome
// supplied by the engine fills the declaration through the detail/status paths.
// Retry selection itself is covered in argojob/status_declared_test.go.
func TestDeclaredRetryOutcomeInRunDetailAndStatus(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "passed", true: "failed"}[failed], func(t *testing.T) {
			at := time.Unix(1000, 0).UTC()
			result := JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{
				{Burn: "retry", Step: "check", Status: model.StepPassed, StartedAt: &at, FinishedAt: &at},
			}}
			if failed {
				result.Status, result.FailedBurn, result.FailedStep = model.RunFailed, "retry", "check"
				result.Steps[0].Status, result.Steps[0].ExitCode = model.StepFailed, 42
			}
			fixture := newControlFixture(t, result)
			fixture.jobs.plan = []runprogress.PlannedStep{{Burn: "retry", Step: "check", Ordinal: 26}}
			ctx := context.Background()
			run, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
				EventID: "retry", Repository: fixture.repo, Branch: "feature/retry",
				SHA: strings.Repeat("a", 40), Actor: "agent@host",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.scheduler.ProcessNext(ctx); err != nil {
				t.Fatal(err)
			}
			// Assign a real indexed range as persistSteps does for replayed logs.
			if err := fixture.scheduler.persistSteps(ctx, run.ID, result.Steps, runlog.Index{Ranges: []runlog.Range{
				{Burn: "retry", Step: "check", Start: 10, End: 100},
			}}); err != nil {
				t.Fatal(err)
			}
			control := fixture.api(t)
			steps := runDetailSteps(t, control, run.ID)
			if len(steps) != 1 || steps[0].Ordinal != 26 || steps[0].Status != result.Steps[0].Status ||
				steps[0].ExitCode != result.Steps[0].ExitCode || steps[0].LogStart != 10 || steps[0].LogEnd != 100 ||
				steps[0].StartedAt == nil || !steps[0].StartedAt.Equal(at) || steps[0].FinishedAt == nil || !steps[0].FinishedAt.Equal(at) {
				t.Fatalf("declared result: %+v", steps)
			}
			status, err := control.status(ctx, fixture.repo.Name, run.SHA, "")
			if err != nil || status.Burns["retry"] != string(result.Steps[0].Status) || len(status.Burns) != 1 {
				t.Fatalf("status: %+v: %v", status, err)
			}
		})
	}
}

var errUnplannable = errors.New("fake engine cannot enumerate this document")

// unwrapPathError lets a test assert "no plan file" through the wrapped error
// StepPlan returns.
func unwrapPathError(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr
	}
	return err
}

// PlannedSteps makes the fake engine a PipelinePlanner, so these tests drive
// the same seam the Argo engine does rather than writing the plan file by hand.
func (jobs *fakeJobs) PlannedSteps(context.Context, JobRequest) ([]runprogress.PlannedStep, error) {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	if jobs.planErr != nil {
		return nil, jobs.planErr
	}
	return append([]runprogress.PlannedStep(nil), jobs.plan...), nil
}

// oberthLikePlan is the shape of a real pipeline: a burn of several steps, then
// burns of one, with two independent builds at the end that a failure earlier
// in the DAG will never reach.
func oberthLikePlan() []runprogress.PlannedStep {
	return []runprogress.PlannedStep{
		{Burn: "setup", Step: "tools-dir", Ordinal: 0},
		{Burn: "setup", Step: "link-go", Ordinal: 1},
		{Burn: "lint", Step: "vet", Ordinal: 2},
		{Burn: "test", Step: "test", Ordinal: 3},
		{Burn: "build-amd64", Step: "build-amd64", Ordinal: 4},
		{Burn: "build-arm64", Step: "build-arm64", Ordinal: 5},
	}
}

func stepStates(steps []model.StepResult) string {
	parts := make([]string, 0, len(steps))
	for _, step := range steps {
		parts = append(parts, step.Burn+"/"+step.Step+"="+string(step.Status))
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

// TestSeededPlanIsVisibleBeforeAnyStepStarts is the defect this exists to fix:
// a run that has just been submitted showed "No step results recorded yet"
// because the step list was built purely from what had already happened.
func TestSeededPlanIsVisibleBeforeAnyStepStarts(t *testing.T) {
	fixture := newControlFixture(t)
	fixture.jobs.plan = oberthLikePlan()
	ctx := context.Background()
	enqueued, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-seeded-plan", Repository: fixture.repo,
		Branch: "feature/seeded-plan", SHA: "1111111111111111111111111111111111111111", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := fixture.store.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Record the plan directly to keep this run active: failing the fake engine
	// would terminalize it and project its untouched declarations as skipped.
	if err := fixture.scheduler.recordStepPlan(ctx, JobRequest{Run: run}); err != nil {
		t.Fatal(err)
	}
	planned, err := fixture.logs.StepPlan(enqueued.ID)
	if err != nil {
		t.Fatalf("the run recorded no plan: %v", err)
	}
	if len(planned) != len(oberthLikePlan()) {
		t.Fatalf("plan = %#v", planned)
	}
	control := fixture.api(t)
	steps := runDetailSteps(t, control, enqueued.ID)
	if len(steps) != len(oberthLikePlan()) {
		t.Fatalf("step list = %s; want all %d planned steps", stepStates(steps), len(oberthLikePlan()))
	}
	for _, step := range steps {
		if step.Status != model.StepPending {
			t.Fatalf("step %s/%s = %q, want pending", step.Burn, step.Step, step.Status)
		}
	}
	// Plan order, not observation order.
	if got := stepStates(steps); !strings.HasPrefix(got, "setup/tools-dir=pending setup/link-go=pending") {
		t.Fatalf("step list is out of plan order: %s", got)
	}
}

// TestPlannedStepsThatNeverRanReportSkipped retains unreached branches on a
// terminal run without suggesting that they are still waiting to execute.
func TestPlannedStepsThatNeverRanReportSkipped(t *testing.T) {
	fixture := newControlFixture(t, JobResult{
		Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "test",
		Steps: []model.StepResult{
			{Burn: "setup", Step: "tools-dir", Status: model.StepPassed},
			{Burn: "setup", Step: "link-go", Status: model.StepPassed},
			{Burn: "lint", Step: "vet", Status: model.StepPassed},
			{Burn: "test", Step: "test", Status: model.StepFailed, ExitCode: 2},
		},
	})
	fixture.jobs.plan = oberthLikePlan()
	ctx := context.Background()
	const sha = "2222222222222222222222222222222222222222"
	enqueued, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-partial-plan", Repository: fixture.repo,
		Branch: "feature/partial-plan", SHA: sha, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	control := fixture.api(t)
	steps := runDetailSteps(t, control, enqueued.ID)
	want := "setup/tools-dir=passed setup/link-go=passed lint/vet=passed test/test=failed " +
		"build-amd64/build-amd64=skipped build-arm64/build-arm64=skipped"
	if got := stepStates(steps); got != want {
		t.Fatalf("step list =\n  %s\nwant\n  %s", got, want)
	}
	status, err := control.status(ctx, fixture.repo.Name, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	for burn, wanted := range map[string]string{
		"setup": "passed", "lint": "passed", "test": "failed",
		"build-amd64": "-", "build-arm64": "-",
	} {
		if status.Burns[burn] != wanted {
			t.Errorf("burn %q = %q, want %q (burns = %#v)", burn, status.Burns[burn], wanted, status.Burns)
		}
	}
	if status.ExitCode == nil || *status.ExitCode != 2 {
		t.Fatalf("exit code = %v, want 2", status.ExitCode)
	}
}

// TestObservedStepsUpdateSeededRowsInPlace proves the count is the planned
// count for the whole life of the run, rather than however many steps happened
// to have started.
func TestObservedStepsUpdateSeededRowsInPlace(t *testing.T) {
	fixture := newControlFixture(t)
	fixture.jobs.plan = oberthLikePlan()
	ctx := context.Background()
	enqueued, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-inplace-plan", Repository: fixture.repo,
		Branch: "feature/inplace-plan", SHA: "3333333333333333333333333333333333333333", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fixture.logs.WriteStepPlan(enqueued.ID, oberthLikePlan()); err != nil {
		t.Fatal(err)
	}
	control := fixture.api(t)
	journal := strings.Join([]string{
		`{"version":1,"burn":"setup","step":"tools-dir","ordinal":0,"status":"passed","declared_size":"M",` +
			`"started_at":"2026-08-11T12:00:00Z","finished_at":"2026-08-11T12:00:05Z"}`,
		`{"version":1,"burn":"setup","step":"link-go","ordinal":1,"status":"running","declared_size":"M",` +
			`"started_at":"2026-08-11T12:00:06Z"}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(fixture.root, "logs", enqueued.ID+".progress.jsonl"), []byte(journal), 0o640); err != nil {
		t.Fatal(err)
	}
	steps := runDetailSteps(t, control, enqueued.ID)
	want := "setup/tools-dir=passed setup/link-go=running lint/vet=pending test/test=pending " +
		"build-amd64/build-amd64=pending build-arm64/build-arm64=pending"
	if got := stepStates(steps); got != want {
		t.Fatalf("step list =\n  %s\nwant\n  %s", got, want)
	}
	if len(steps) != len(oberthLikePlan()) {
		t.Fatalf("live step count = %d, want the planned %d", len(steps), len(oberthLikePlan()))
	}
}

// TestInterruptedRunRetainsItsPlanAndTheProgressItMade is the second defect the
// plan closes. persistSteps only ever runs from a completed engine result, and
// the interrupted path builds an empty one -- so a run cancelled mid-flight
// discarded every step it had visibly executed and returned Steps: null.
func TestInterruptedRunRetainsItsPlanAndTheProgressItMade(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	enqueued, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-interrupted-plan", Repository: fixture.repo,
		Branch: "feature/interrupted-plan", SHA: "4444444444444444444444444444444444444444", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fixture.logs.WriteStepPlan(enqueued.ID, oberthLikePlan()); err != nil {
		t.Fatal(err)
	}
	journal := strings.Join([]string{
		`{"version":1,"burn":"setup","step":"tools-dir","ordinal":0,"status":"passed","declared_size":"M",` +
			`"started_at":"2026-08-11T12:00:00Z","finished_at":"2026-08-11T12:00:05Z"}`,
		`{"version":1,"burn":"setup","step":"link-go","ordinal":1,"status":"running","declared_size":"M",` +
			`"started_at":"2026-08-11T12:00:06Z"}`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(fixture.root, "logs", enqueued.ID+".progress.jsonl"), []byte(journal), 0o640); err != nil {
		t.Fatal(err)
	}
	// Exactly what the shutdown path records: interrupted, with no steps.
	if _, err := fixture.store.FinishRun(ctx, enqueued.ID, model.RunResult{
		Status: model.RunInterrupted, Phase: "interrupted", Error: "scheduler stopped",
	}); err != nil {
		t.Fatal(err)
	}
	steps := runDetailSteps(t, fixture.api(t), enqueued.ID)
	want := "setup/tools-dir=passed setup/link-go=running lint/vet=skipped test/test=skipped " +
		"build-amd64/build-amd64=skipped build-arm64/build-arm64=skipped"
	if got := stepStates(steps); got != want {
		t.Fatalf("interrupted run step list =\n  %s\nwant\n  %s", got, want)
	}
}

// TestRunWithoutAPlanKeepsItsExistingBehaviour holds the compatibility line:
// every run submitted before plans existed, and every engine that cannot
// enumerate its own document, reports exactly what it always did.
func TestRunWithoutAPlanKeepsItsExistingBehaviour(t *testing.T) {
	fixture := newControlFixture(t, JobResult{
		Status: model.RunPassed, Phase: "passed",
		Steps: []model.StepResult{
			{Burn: "test", Step: "unit", Status: model.StepPassed},
			{Burn: "test", Step: "race", Status: model.StepPassed},
		},
	})
	fixture.jobs.plan = nil
	ctx := context.Background()
	enqueued, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-planless", Repository: fixture.repo,
		Branch: "feature/planless", SHA: "5555555555555555555555555555555555555555", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.logs.StepPlan(enqueued.ID); !os.IsNotExist(unwrapPathError(err)) {
		t.Fatalf("a plan was written for an engine that reported none: %v", err)
	}
	steps := runDetailSteps(t, fixture.api(t), enqueued.ID)
	if got := stepStates(steps); got != "test/unit=passed test/race=passed" {
		t.Fatalf("step list = %s", got)
	}
}

// TestUnenumerablePipelineStillRuns is the rule that a visibility record is
// never a gate: a document whose plan cannot be read fails the plan, not the
// build, and says so in the run's own log.
func TestUnenumerablePipelineStillRuns(t *testing.T) {
	fixture := newControlFixture(t, JobResult{
		Status: model.RunPassed, Phase: "passed",
		Steps: []model.StepResult{{Burn: "test", Step: "unit", Status: model.StepPassed}},
	})
	fixture.jobs.planErr = errUnplannable
	ctx := context.Background()
	enqueued, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
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
	if !strings.Contains(string(body), "step plan unavailable") {
		t.Fatalf("the run log does not report the missing plan: %q", body)
	}
}

// TestWireBurnsDoesNotDependOnStepOrder guards the burns projection against the
// order its input happens to arrive in. A burn with a passed step and a step
// that was never reached is not a passed burn.
func TestWireBurnsDoesNotDependOnStepOrder(t *testing.T) {
	forward := []model.StepResult{
		{Burn: "setup", Step: "one", Status: model.StepPassed},
		{Burn: "setup", Step: "two", Status: model.StepPending},
	}
	reversed := []model.StepResult{forward[1], forward[0]}
	for name, steps := range map[string][]model.StepResult{"forward": forward, "reversed": reversed} {
		burns, _ := wireBurns(steps, model.Run{})
		if burns["setup"] != "pending" {
			t.Errorf("%s: burn = %q, want pending", name, burns["setup"])
		}
	}
	burns, _ := wireBurns([]model.StepResult{
		{Burn: "b", Step: "skipped", Status: model.StepSkipped},
		{Burn: "b", Step: "passed", Status: model.StepPassed},
	}, model.Run{})
	if burns["b"] != "passed" {
		t.Errorf("a burn of one passed and one skipped step = %q, want passed", burns["b"])
	}
	burns, _ = wireBurns([]model.StepResult{
		{Burn: "c", Step: "pending", Status: model.StepPending},
		{Burn: "c", Step: "failed", Status: model.StepFailed},
	}, model.Run{})
	if burns["c"] != "failed" {
		t.Errorf("a burn containing a failure = %q, want failed", burns["c"])
	}
}
