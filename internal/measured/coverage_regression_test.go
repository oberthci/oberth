package measured

import (
	"strings"
	"testing"
	"time"
)

func TestAdviceKnownCensorSurvivesNonterminalTelemetry(t *testing.T) {
	rows := observations()
	assertCoveredAdvice(t, rows)
	partial := rows[0]
	partial.Usage.RunID = "failed-run"
	partial.Usage.PodUID = "failed-pod"
	partial.Usage.ContainerID = "failed-container"
	partial.Usage.Terminal = false
	partial.Usage.ContainerFinishedAt = time.Time{}
	partial.DeadlineKilled = true
	step := Build(append(rows, partial), nil)[0]
	if step.Suggested != nil || !step.Flags.DeadlineKilled {
		t.Fatalf("known censor disappeared before safety gating: accepted=%t deadline_killed=%t attempts=%d", step.Suggested != nil, step.Flags.DeadlineKilled, step.Attempts)
	}
}

func TestAdviceSparseStorageCannotEstablishLocalLimit(t *testing.T) {
	rows := observations()
	assertCoveredAdvice(t, rows)
	for i := range rows {
		rows[i].Usage.Samples = 4
		rows[i].Usage.StorageCoverage.Count = 1
		rows[i].StepMemoryPeakBytes = value(512 << 20)
		rows[i].Usage.EphemeralStoragePeakBytes = value(180224)
		rows[i].EphemeralLimitBytes = value(1 << 30)
	}
	step := Build(rows, nil)[0]
	if step.StorageInsufficientAttempts != 5 || !strings.Contains(strings.Join(step.NoSuggestion, " "), "storage") {
		t.Fatalf("wrong refusal: %+v", step)
	}
	if step.Suggested != nil {
		t.Fatalf("five one-poll storage lower bounds accepted: storage_limit=%d sample_max=%d step_memory_attempts=%d", step.Suggested.EphemeralLimitBytes, *step.EphemeralBytes.Max, step.MemoryStepAttempts)
	}
}

func TestAdviceFailedWorkloadDoesNotProveCompletionBound(t *testing.T) {
	rows := observations()
	assertCoveredAdvice(t, rows)
	rows[0].Usage.Reason = "Error"
	code := int32(1)
	rows[0].Usage.ExitCode = &code
	step := Build(rows, nil)[0]
	if step.Flags.OOM || step.SizingAcceptance.Status == "censored" || step.FailedWorkloadAttempts != 1 || step.MemoryPeakBytes.Count != 5 {
		t.Fatalf("failure semantics/measurements changed: %+v", step)
	}
	if step.Suggested != nil {
		t.Fatalf("failed workload counted as complete sizing evidence: complete_runs=%d", step.SizingAcceptance.ComparableCompleteRuns)
	}
}

func TestAdviceEarlyCPUCounterDoesNotCoverTerminalTail(t *testing.T) {
	for _, seconds := range []int64{1, 4} {
		rows := observations()
		assertCoveredAdvice(t, rows)
		for i := range rows {
			v := &rows[i].Usage
			v.CPUTimeAt = v.ContainerStartedAt.Add(time.Duration(seconds) * time.Second)
			v.CPUTimeNS = value(seconds * 500_000_000)
			v.CPUCoverage.Count = seconds
			v.CPUCoverage.FirstAt = v.ContainerStartedAt.Add(time.Second)
			v.CPUCoverage.LastAt = v.CPUTimeAt
		}
		step := Build(rows, nil)[0]
		if step.CPUInsufficientAttempts != 5 || step.AverageCPUNanoCores.Count != 5 || *step.AverageCPUNanoCores.Max != 500_000_000 || *step.DurationNS.Max != int64(10*time.Second) {
			t.Fatalf("%d-second coverage lost numeric measurements or CPU refusal: %+v", seconds, step)
		}
		if step.Suggested != nil {
			t.Fatalf("%d-second CPU counter accepted for ten-second workload: avg=%d request_milli=%d", seconds, *step.AverageCPUNanoCores.Max, step.Suggested.CPUMilli)
		}
	}
}

func TestAdviceUnknownCacheCoverageCannotJustifyReductions(t *testing.T) {
	declaration := Declaration{Stage: "step-12345678", Template: "step-abc12345", Resources: map[string]map[string]string{"limits": {"memory": "2Gi", "ephemeral-storage": "512Mi"}}}
	step := Build(observations(), []Declaration{declaration})[0]
	if step.Suggested == nil {
		t.Fatalf("positive provisional advice lost: %+v", step)
	}
	if step.AdviceBasis != ProvisionalAdvice {
		t.Fatal("missing provisional/cache qualification")
	}
	if step.Suggested.MemoryLimitBytes < 2<<30 || step.Suggested.EphemeralLimitBytes < 512<<20 {
		t.Fatalf("unknown cache/workload coverage offers a reduction: memory_limit=%d storage_limit=%d", step.Suggested.MemoryLimitBytes, step.Suggested.EphemeralLimitBytes)
	}
}

func assertCoveredAdvice(t *testing.T, rows []Observation) {
	t.Helper()
	step := Build(rows, nil)[0]
	if step.Suggested == nil {
		t.Fatalf("otherwise-eligible positive control refused: %+v", step)
	}
}
