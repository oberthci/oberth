package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
)

var (
	errWaitForever = errors.New("fake engine never completes")
	errUnplannable = errors.New("fake engine cannot enumerate this document")
)

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
	// The engine reports nothing, so no step ever ran: the step list can only
	// be the plan.
	fixture.jobs.waitErr = errWaitForever
	if err := fixture.scheduler.ProcessNext(ctx); err != nil {
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

// TestPlannedStepsThatNeverRanReportPendingRatherThanVanishing is the agent-
// facing half. Before the plan, `status` on a run that failed in test returned
// a burns map with build-amd64 and build-arm64 simply absent -- a plausible
// complete pipeline that silently omitted the work that never ran.
func TestPlannedStepsThatNeverRanReportPendingRatherThanVanishing(t *testing.T) {
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
		"build-amd64/build-amd64=pending build-arm64/build-arm64=pending"
	if got := stepStates(steps); got != want {
		t.Fatalf("step list =\n  %s\nwant\n  %s", got, want)
	}
	status, err := control.status(ctx, fixture.repo.Name, sha, "")
	if err != nil {
		t.Fatal(err)
	}
	for burn, wanted := range map[string]string{
		"setup": "passed", "lint": "passed", "test": "failed",
		"build-amd64": "pending", "build-arm64": "pending",
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
	want := "setup/tools-dir=passed setup/link-go=running lint/vet=pending test/test=pending " +
		"build-amd64/build-amd64=pending build-arm64/build-arm64=pending"
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
