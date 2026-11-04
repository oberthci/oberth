package service

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestRunListMessageContract(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		run  model.Run
		want string
	}{
		{"queued", model.Run{Status: model.RunQueued}, "next in line"},
		{"tag boost", model.Run{Status: model.RunRunning, StartedAt: &now, Priority: model.PriorityHighest, PriorityReason: model.TagReleasePriorityReason}, ""},
		{"aged", model.Run{Status: model.RunRunning, StartedAt: &now, Priority: model.PriorityHigh, PriorityReason: model.PriorityAgedReason}, "priority high (aged)"},
		{"running", model.Run{Status: model.RunRunning, StartedAt: &now, Priority: "normal", PriorityReason: "ordinary run"}, ""},
		{"promotion", model.Run{Status: model.RunRunning, StartedAt: &now, Priority: "highest", PriorityReason: "promotion run"}, ""},
		{"admin priority", model.Run{Status: model.RunRunning, StartedAt: &now, Priority: "highest", PriorityReason: "admin bump: release candidate"}, "priority highest · release candidate"},
		{"publishing", model.Run{Status: model.RunRunning, StartedAt: &now, Phase: "publishing"}, "publishing to upstream"},
		{"passed", model.Run{Status: model.RunPassed}, ""},
		{"schedule", model.Run{Status: model.RunPassed, Trigger: "schedule", ScheduleEntry: "nightly"}, "schedule nightly"},
		{"failed", model.Run{Status: model.RunFailed, Phase: "test", FailedStage: "test", FailedStep: "unit-tests", Error: "unit-tests: Workflow failed"}, "test/unit-tests failed"},
		{"infrastructure", model.Run{Status: model.RunFailed, Phase: "infrastructure", Error: "controller unavailable"}, "infrastructure error: controller unavailable"},
		{"cancelled", model.Run{Status: model.RunInterrupted, Phase: model.PhaseCancelled, Reason: "cancelled by owner: no longer needed"}, "by owner: no longer needed"},
		{"superseded", model.Run{Status: model.RunInterrupted, SupersededBy: "successor", Reason: "superseded by newer run"}, "superseded by newer run"},
		{"infra-retried", model.Run{Status: model.RunFailed, AutoRetriedBy: "abcdef0123456789"}, "retried as abcdef012345"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := presentationRunAt(tc.run, now).DisplayReason; got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	long := presentationRun(model.Run{Status: model.RunFailed, Error: strings.Repeat("x", 200)}).DisplayReason
	if len([]rune(long)) > 60 {
		t.Errorf("message has %d characters", len([]rune(long)))
	}
}

func TestRunListQueuedResourcesAndQualifier(t *testing.T) {
	admission := &model.AdmissionObservation{State: "waiting", DeclaredResources: &model.DeclaredResources{CPUMilli: 9900, MemoryBytes: 16 << 30}, Pressure: &model.PressureDeferral{Resource: "io"}, BlockedBy: &model.AdmissionBlocker{Repo: "wrapper", Ref: "main"}}
	got := queuedDisplayReason(&model.RunScheduling{State: "waiting", Admission: admission}, nil)
	if want := "waiting for 9.9 CPU, 16 GiB · host IO pressure"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRunListOmitsAdviceWhileDetailRetainsIt(t *testing.T) {
	f := newControlFixture(t)
	run, err := f.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/message", SHA: strings.Repeat("a", 40), Actor: "owner", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	reader := &coverageReaderStub{}
	service := f.api(t)
	service.resourceCoverages = reader
	actor := api.Actor{Identity: "owner"}
	value, err := service.Runs(t.Context(), actor, api.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	rows := value.([]model.Run)
	if len(rows) != 1 || rows[0].ResourceMessage != "" || reader.calls != 0 {
		t.Fatalf("list read detail advice: %+v, %d calls", rows, reader.calls)
	}
	detail, err := service.Run(t.Context(), actor, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.(RunDetailResponse).Run.ResourceMessage == "" || reader.calls == 0 {
		t.Fatal("detail lost advice")
	}
}

func TestRunListPersistsTypedCeilingCauseAndFullError(t *testing.T) {
	f := newControlFixture(t)
	run, err := f.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/ceiling", SHA: strings.Repeat("a", 40), Actor: "owner", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	failure := &argoworkflow.ResourceCeilingError{Detail: "full refusal with resource grant remediation", Cause: "cpu limit 5994m over ceiling 4"}
	if err := f.scheduler.finishInfrastructureFailure(t.Context(), run.Run, f.repo, failure); err != nil {
		t.Fatal(err)
	}
	stored, err := f.store.Run(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Error != failure.Error() || presentationRun(stored).DisplayReason != "infrastructure error: cpu limit 5994m over ceiling 4" {
		t.Fatalf("typed cause or evidence lost: %+v", stored)
	}
}

func TestRunListWaitQualifierPrivacyAndClipping(t *testing.T) {
	a := &model.AdmissionObservation{DeclaredResources: &model.DeclaredResources{CPUMilli: 9900, MemoryBytes: 16 << 30}, BlockedBy: &model.AdmissionBlocker{}}
	if got := waitingReason(a); got != "waiting for 9.9 CPU, 16 GiB" {
		t.Fatal(got)
	}
	a.BlockedBy = &model.AdmissionBlocker{Repo: strings.Repeat("界", 80), Ref: "main"}
	message := waitingReason(a)
	if len([]rune(message)) != 60 || !strings.HasSuffix(message, "…") {
		t.Fatal(message)
	}
	run := model.Run{Status: model.RunQueued, DisplayStatus: model.DisplayQueued, Priority: model.PriorityHighest, PriorityReason: "admin bump: release"}
	applyQueuedDisplay(&run, &model.RunScheduling{State: "queued", QueuePosition: 2}, "")
	if run.DisplayReason != "priority highest · position 2 in line" {
		t.Fatal(run.DisplayReason)
	}
}

func TestRunListPendingPodMessage(t *testing.T) {
	now := time.Now()
	run := presentationRun(model.Run{Status: model.RunRunning, StartedAt: &now})
	applyExecutionDisplay(&run, &model.RunScheduling{Execution: &model.ExecutionObservation{Pods: []model.SchedulingReason{{Reason: "Unschedulable", Message: "pod pending: insufficient cpu (4 CPU requested)"}}}})
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
	step := model.StepResult{RunID: runID, Stage: "test", Step: "unit-tests", Status: model.StepFailed, Termination: diagnostic}
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
	if got := structuredFailureCause(strings.Repeat("c", 32), result); got != "" {
		t.Fatal("foreign run evidence accepted", got)
	}
	result.Steps[0].Status = model.StepPassed
	if got := structuredFailureCause(runID, result); got != "" {
		t.Fatal("successful retry tainted", got)
	}
}
