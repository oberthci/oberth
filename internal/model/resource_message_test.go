package model

import (
	"strings"
	"testing"
	"time"
)

func TestResourceFailureMessageUsesBoundDiagnosticsWithoutUsage(t *testing.T) {
	run := strings.Repeat("a", 32)
	limit := int64(1 << 30)
	at := time.Now().UTC()
	c := &StepTermination{RunID: run, WorkflowUID: "11111111-1111-4111-8111-111111111111", Namespace: "argo", WorkflowName: "workflow-test", NodeID: "node-1", PodName: "pod-1", PodUID: "22222222-2222-4222-8222-222222222222", ContainerName: "main", ContainerID: "containerd://" + strings.Repeat("b", 64), Reason: "OOMKilled", PodReason: "none", ExitCode: 137, StartedAt: at.Add(-10 * time.Second), FinishedAt: at, MemoryLimitBytes: &limit}
	step := StepResult{RunID: run, Stage: "build", Step: "test", Status: StepFailed, Termination: &StepDiagnostic{Available: true, ObservedAt: &at, Container: c}}
	got := ResourceFailureMessage(run, []StepResult{step}, false)
	if !strings.Contains(got, "1Gi") || !strings.Contains(got, "build/test") || strings.Contains(got, "peak") || strings.Contains(got, "run resources") {
		t.Fatal(got)
	}
	step.Status = StepPassed
	if got := ResourceFailureMessage(run, []StepResult{step}, false); got != "" {
		t.Fatal("old OOM tainted successful retry", got)
	}
}
func TestResourceFailureDeadlineKeepsDeadlineClassificationAndUnknowns(t *testing.T) {
	got := ResourceFailureMessage(strings.Repeat("a", 32), []StepResult{{Stage: "step-12345678", Step: "step-abc12345", Status: StepTimedOut}}, true)
	if !IsExecutionDeadlineReason(got) || !strings.Contains(got, "limit unknown") || !strings.Contains(got, "step-abc12345") {
		t.Fatal(got)
	}
}

func TestResourceFailureGlobalDeadlineDoesNotBlameArbitraryFailedStep(t *testing.T) {
	steps := []StepResult{{Stage: "step-00000000", Step: "step-11111111", Status: StepFailed}, {Stage: "step-00000000", Step: "step-22222222", Status: StepFailed}}
	if got := ResourceFailureMessage(strings.Repeat("a", 32), steps, true); !strings.Contains(got, "Step unknown") || strings.Contains(got, "step-11111111") || strings.Contains(got, "step-22222222") {
		t.Fatal("ambiguous deadline step attributed", got)
	}
}
