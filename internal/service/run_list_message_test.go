package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type selectedNodeHistory struct {
	RunHistory
	queries int
}

func (history *selectedNodeHistory) StepResults(context.Context, string) ([]model.StepResult, error) {
	panic("failure presentation must not load the full native node tree")
}

func (history *selectedNodeHistory) StepResult(ctx context.Context, runID, nodeID string) (model.StepResult, error) {
	history.queries++
	return history.RunHistory.StepResult(ctx, runID, nodeID)
}

func TestRunListMessageContract(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		run  model.Run
		want string
	}{
		{"queued", model.Run{Status: model.RunQueued}, "starting"},
		{"tag boost", model.Run{Status: model.RunRunning, StartedAt: &now}, ""},
		{"running", model.Run{Status: model.RunRunning, StartedAt: &now}, ""},
		{"promotion", model.Run{Status: model.RunRunning, StartedAt: &now}, ""},
		{"publishing", model.Run{Status: model.RunRunning, StartedAt: &now, Phase: "publishing"}, "publishing to upstream"},
		{"passed", model.Run{Status: model.RunPassed}, ""},
		{"schedule", model.Run{Status: model.RunPassed, Trigger: "schedule", ScheduleEntry: "nightly"}, "schedule nightly"},
		{"failed", model.Run{Status: model.RunFailed, Phase: "test", FailedStage: "test", FailedStep: "unit-tests", Error: "unit-tests: Workflow failed"}, "unit-tests failed"},

		{"same stage and step", model.Run{Status: model.RunFailed, FailedStage: "contract-guard", FailedStep: "contract-guard"}, "contract-guard failed"},
		{"release leaf", model.Run{Status: model.RunFailed, FailedStage: "release", FailedStep: "release-index"}, "release-index failed"},
		{"stage only", model.Run{Status: model.RunFailed, FailedStage: "test"}, "test failed"},
		{"no identity", model.Run{Status: model.RunFailed}, "failed"},
		{"historical OOM", model.Run{Status: model.RunFailed, FailedStage: "test", FailedStep: "unit-tests", FailureCause: "test/unit-tests failed · OOM (limit 2 GiB)"}, "unit-tests failed · OOM (limit 2 GiB)"},
		{"historical stage only", model.Run{Status: model.RunFailed, FailedStage: "test", FailureCause: "test/ failed · OOM"}, "test failed · OOM"},
		{"historical no identity", model.Run{Status: model.RunFailed, FailureCause: "/ failed · deadline"}, "failed · deadline"},
		{"long leaf", model.Run{Status: model.RunFailed, FailedStage: "test", FailedStep: strings.Repeat("界", 80)}, strings.Repeat("界", 59) + "…"},
		{"historical deadline", model.Run{Status: model.RunFailed, FailedStage: "test", FailedStep: "e2e", FailureCause: "test/e2e failed · deadline 20m"}, "e2e failed · deadline 20m"},
		{"unstructured path", model.Run{Status: model.RunFailed, FailedStage: "test", FailedStep: "unit-tests", FailureCause: "cannot read /test/unit-tests/file"}, "cannot read /test/unit-tests/file"},
		{"foreign identity", model.Run{Status: model.RunFailed, FailedStage: "test", FailedStep: "unit-tests", FailureCause: "other/unit-tests failed · OOM"}, "other/unit-tests failed · OOM"},
		{"arbitrary suffix", model.Run{Status: model.RunFailed, FailedStage: "test", FailedStep: "unit-tests", FailureCause: "test/unit-tests failed opening /path"}, "test/unit-tests failed opening /path"},
		{"clipped historical prefix", model.Run{Status: model.RunFailed, FailedStage: strings.Repeat("界", 65), FailedStep: "leaf", FailureCause: displayFailureText(strings.Repeat("界", 65) + "/leaf failed · OOM")}, "leaf failed"},
		{"infrastructure", model.Run{Status: model.RunFailed, Phase: "infrastructure", Error: "controller unavailable"}, "infrastructure error: controller unavailable"},
		{"cancelled", model.Run{Status: model.RunInterrupted, Phase: model.PhaseCancelled, Reason: "cancelled by owner: no longer needed"}, "by owner: no longer needed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.run
			shown := presentationRunAt(tc.run, now)
			if shown.FailedStage != before.FailedStage || shown.FailedStep != before.FailedStep || shown.FailureCause != before.FailureCause || shown.Error != before.Error {
				t.Fatal("presentation changed retained failure identity or evidence")
			}
			if got := shown.DisplayReason; got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	long := presentationRun(model.Run{Status: model.RunFailed, Error: strings.Repeat("x", 200)}).DisplayReason
	if len([]rune(long)) > 60 {
		t.Errorf("message has %d characters", len([]rune(long)))
	}
}

func TestRunListPendingPodMessage(t *testing.T) {
	now := time.Now()
	run := presentationRun(model.Run{Status: model.RunRunning, StartedAt: &now})
	applyExecutionDisplay(&run, &model.RunScheduling{Execution: &model.ExecutionObservation{Pods: []model.SchedulingReason{{Phase: "Pending", Reason: "Unschedulable", Message: "pod pending: insufficient cpu (4 CPU requested)"}}}})
	if run.DisplayReason != "pod pending: insufficient cpu (4 CPU requested)" {
		t.Fatal(run.DisplayReason)
	}
}

func TestStructuredFailureCauseUsesBoundDiagnostics(t *testing.T) {
	now := time.Now().UTC()
	runID := strings.Repeat("a", 32)
	limit, deadline := int64(2<<30), int64(1200)
	container := &model.StepTermination{RunID: runID, Namespace: "pipeline", WorkflowName: "workflow", WorkflowUID: "11111111-1111-4111-8111-111111111111", NodeID: "unit-tests", PodName: "unit-tests", PodUID: "22222222-2222-4222-8222-222222222222", ContainerName: "main", ContainerID: "containerd://" + strings.Repeat("b", 64), Reason: "OOMKilled", PodReason: "none", ExitCode: 137, StartedAt: now.Add(-time.Minute), FinishedAt: now, MemoryLimitBytes: &limit, ActiveDeadlineSeconds: &deadline}
	diagnostic := &model.StepDiagnostic{Available: true, ObservedAt: &now, Container: container}
	if err := diagnostic.Validate(); err != nil {
		t.Fatal(err)
	}
	step := model.StepResult{NodeID: "test.unit-tests", RunID: runID, Stage: "test", Step: "unit-tests", Status: model.StepFailed, Termination: diagnostic}
	result := JobResult{Status: model.RunFailed, Steps: []model.StepResult{step}}
	if got := structuredFailureCause(runID, result); got != "test/unit-tests failed · OOM (limit 2 GiB)" {
		t.Fatal(got)
	}
	container.Reason, container.PodReason = "Error", "DeadlineExceeded"
	result.Steps[0].Step = "e2e"
	result.Steps[0].Status = model.StepTimedOut
	if got := structuredFailureCause(runID, result); got != "test/e2e failed · deadline 20m" {
		t.Fatal(got)
	}
	// Historical summaries can end at any point inside the structured suffix.
	// Exercise the actual producer's 60-rune clipping, then the list projection.
	for _, tc := range []struct {
		name   string
		size   int
		oom    bool
		suffix string
	}{
		{"clipped OOM word", 39, true, ""},
		{"complete OOM word", 38, true, " · OOM"},
		{"clipped limit label", 34, true, " · OOM"},
		{"clipped limit value", 28, true, " · OOM"},
		{"clipped deadline word", 35, false, ""},
		{"complete deadline word", 33, false, " · deadline"},
		{"clipped deadline value", 31, false, " · deadline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := *container
			c.Reason, c.PodReason = "Error", "DeadlineExceeded"
			status := model.StepTimedOut
			if tc.oom {
				c.Reason, c.PodReason, status = "OOMKilled", "none", model.StepFailed
			}
			d := *diagnostic
			d.Container = &c
			name := strings.Repeat("a", tc.size)
			cause := structuredFailureCause(runID, JobResult{Status: model.RunFailed, Steps: []model.StepResult{{NodeID: ("compile") + "." + (name), RunID: runID, Stage: "compile", Step: name, Status: status, Termination: &d}}})
			if len([]rune(cause)) != 60 || !strings.HasSuffix(cause, "…") {
				t.Fatalf("fixture did not clip: %q", cause)
			}
			run := model.Run{Status: model.RunFailed, FailedStage: "compile", FailedStep: name, FailureCause: cause}
			shown := presentationRun(run)
			if want := name + " failed" + tc.suffix; shown.DisplayReason != want {
				t.Fatalf("cause %q rendered %q, want %q", cause, shown.DisplayReason, want)
			}
			if shown.FailureCause != cause || shown.FailedStage != run.FailedStage || shown.FailedStep != name {
				t.Fatal("historical evidence changed")
			}
		})
	}
	for _, raw := range []string{
		"compile/" + strings.Repeat("a", 39) + " failed opening /tmp/some/path",
		"foreign/" + strings.Repeat("a", 39) + " failed · OOM (limit 2 GiB)",
	} {
		cause := displayFailureText(raw)
		shown := presentationRun(model.Run{Status: model.RunFailed, FailedStage: "compile", FailedStep: strings.Repeat("a", 39), FailureCause: cause})
		if shown.DisplayReason != cause {
			t.Fatalf("unstructured or foreign clipped text changed: %q -> %q", cause, shown.DisplayReason)
		}
	}
	if got := structuredFailureCause(strings.Repeat("c", 32), result); got != "" {
		t.Fatal("foreign run evidence accepted", got)
	}
	result.Steps[0].Status = model.StepPassed
	if got := structuredFailureCause(runID, result); got != "" {
		t.Fatal("successful retry tainted", got)
	}
}

func TestNativeFailedNodeDisplayAcrossListHintAndAssist(t *testing.T) {
	f := newRetentionFixture(t)
	ctx := t.Context()
	queued, err := f.retention.Database.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/display", SHA: strings.Repeat("a", 40), Actor: "reader", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	row := model.StepResult{RunID: queued.ID, NodeID: "workflow-3810230957", DisplayName: "build (retry 1)", Step: "build", Stage: "workflow", NodeType: "Pod", Phase: "Failed", Status: model.StepFailed, ExitCode: 1}
	if err := f.retention.Database.PutStepResults(ctx, row.RunID, []model.StepResult{row}); err != nil {
		t.Fatal(err)
	}
	run, err := f.retention.Database.FinishRun(ctx, row.RunID, model.RunResult{Status: model.RunFailed, Phase: "workflow", FailedStage: "workflow", FailedStep: row.NodeID})
	if err != nil {
		t.Fatal(err)
	}
	history := &selectedNodeHistory{RunHistory: f.api.history}
	f.api.history = history
	shown := f.api.presentationRun(ctx, run)
	list, err := f.api.presentationRuns(ctx, []model.Run{run})
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	if history.queries != 2 {
		t.Fatal("failure display did not select one row per run", history.queries)
	}
	for _, visible := range []model.Run{shown, list[0]} {
		if visible.FailedStep != row.NodeID || visible.DisplayReason != "build (retry 1) failed" {
			t.Fatal("failure identity or display changed", visible)
		}
		hint := runRetryHint(visible, nil)
		if hint == nil || hint.Detail != "step workflow/build (retry 1) failed" {
			t.Fatal(hint)
		}
	}
	run = run.WithFailedNode([]model.StepResult{row})
	run.FailureCause = "workflow/build failed · OOM"
	if got := failedRunDisplayReason(run); got != "build (retry 1) failed · OOM" {
		t.Fatal("structured cause did not resolve native failure name", got)
	}
	run.FailureCause = ""
	run.Assist = &model.RunAssistRecord{Events: []model.RunAssistEvent{{Step: "build", Kind: model.RunAssistAbandon, AbandonReason: model.AssistAbandonCap}}}
	if got := failedRunDisplayReason(run); got != "" {
		t.Fatal("assist event did not match native step name", got)
	}
	row.DisplayName = ""
	if got := run.WithFailedNode([]model.StepResult{row}).FailedStepDisplayName(); got != "build" {
		t.Fatal("missing display name did not use step", got)
	}
}
