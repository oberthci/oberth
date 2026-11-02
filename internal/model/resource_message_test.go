package model

import (
	"strings"
	"testing"
	"time"
)

func TestResourceFailureMessageUsesBoundExternalPeakAndNeverSelfReport(t *testing.T) {
	v := resourceModelRow()
	v.Stage = "step-12345678"
	v.Step = "step-abc12345"
	limit, peak, self := int64(1<<30), int64(800<<20), int64(9<<40)
	at := time.Now().UTC()
	start := at.Add(-10 * time.Second)
	c := &StepTermination{RunID: v.RunID, WorkflowUID: v.WorkflowUID, Namespace: v.Namespace, WorkflowName: v.WorkflowName, NodeID: v.NodeID, PodName: v.PodName, PodUID: v.PodUID, ContainerName: "main", ContainerID: v.ContainerID, Reason: "OOMKilled", PodReason: "none", ExitCode: 137, StartedAt: start, FinishedAt: at, MemoryLimitBytes: &limit}
	step := StepResult{RunID: v.RunID, Stage: v.Stage, Step: v.Step, Status: StepFailed, MemoryPeakBytes: &self, Termination: &StepDiagnostic{Available: true, ObservedAt: &at, Container: c}}
	v.MemoryPeakBytes = &peak
	got := ResourceFailureMessage(v.RunID, []StepResult{step}, []StepResourceUsage{v}, false)
	if !strings.Contains(got, "step-abc12345") || !strings.Contains(got, "1Gi") || !strings.Contains(got, "800Mi") || !strings.Contains(got, "oberth run resources --run "+v.RunID+" --include-failed") || strings.Contains(got, "9Ti") {
		t.Fatal(got)
	}
	v.ContainerID += "f"
	got = ResourceFailureMessage(c.RunID, []StepResult{step}, []StepResourceUsage{v}, false)
	if !strings.Contains(got, "peak unknown") {
		t.Fatal(got)
	}
	step.Status = StepPassed
	if got := ResourceFailureMessage(c.RunID, []StepResult{step}, nil, false); got != "" {
		t.Fatal("old OOM tainted successful retry", got)
	}
}
func TestResourceFailureDeadlineKeepsDeadlineClassificationAndUnknowns(t *testing.T) {
	got := ResourceFailureMessage(strings.Repeat("a", 32), []StepResult{{Stage: "step-12345678", Step: "step-abc12345", Status: StepTimedOut}}, nil, true)
	if !IsExecutionDeadlineReason(got) || !strings.Contains(got, "limit unknown") || !strings.Contains(got, "peak unknown") || !strings.Contains(got, "step-abc12345") {
		t.Fatal(got)
	}
}

func TestResourceFailureGlobalDeadlineDoesNotBlameArbitraryFailedStep(t *testing.T) {
	steps := []StepResult{{Stage: "step-00000000", Step: "step-11111111", Status: StepFailed}, {Stage: "step-00000000", Step: "step-22222222", Status: StepFailed}}
	if got := ResourceFailureMessage(strings.Repeat("a", 32), steps, nil, true); !strings.Contains(got, "Step unknown") || strings.Contains(got, "step-11111111") || strings.Contains(got, "step-22222222") {
		t.Fatal("ambiguous deadline step attributed", got)
	}
}
