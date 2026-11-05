package measured

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestMemorySparsePollsSuppressWholeSuggestion(t *testing.T) {
	for _, polls := range []int64{0, 1, 2, 3} {
		t.Run(string(rune('0'+polls)), func(t *testing.T) {
			rows := observations()
			for i := range rows {
				rows[i].Usage.Samples = 4
			}
			extra := rows[0]
			extra.Usage.RunID = "6"
			extra.Usage.PodUID = "6"
			extra.Usage.ContainerID = "6"
			extra.Usage.Samples = polls
			extra.Usage.MemoryCoverage.Count = polls
			rows = append(rows, extra)
			got := Build(rows, nil)[0]
			if got.Suggested != nil || !got.Flags.LowSamples || got.MemoryInsufficientAttempts != 1 {
				t.Fatalf("sparse attempt: %+v", got)
			}
		})
	}
}
func TestMemoryPrefersBoundStepPeakOverSparseSampler(t *testing.T) {
	rows := observations()
	for i := range rows {
		rows[i].Usage.Samples = 1
		rows[i].Usage.MemoryPeakBytes = value(32 << 20)
		rows[i].StepMemoryPeakBytes = value(1536 << 20)
		rows[i].MemoryLimitBytes = value(4 << 30)
	}
	got := Build(rows, nil)[0]
	if got.Suggested == nil || got.Suggested.MemoryLimitBytes < 2304<<20 || *got.MemoryPeakBytes.Max != 1536<<20 || got.MemoryStepAttempts != 5 {
		t.Fatalf("step peak: %+v", got)
	}
}

func TestMemoryFourPollBoundaryAndHigherExternalPeak(t *testing.T) {
	rows := observations()
	for i := range rows {
		rows[i].StepMemoryPeakBytes = value(128 << 20)
		rows[i].Usage.MemoryPeakBytes = value(512 << 20)
	}
	got := Build(rows, nil)[0]
	if got.Suggested == nil || *got.MemoryPeakBytes.Max != 512<<20 {
		t.Fatalf("higher sufficiently sampled peak lost: %+v", got)
	}
	for i := range rows {
		rows[i].StepMemoryPeakBytes = nil
	}
	got = Build(rows, nil)[0]
	if got.Suggested == nil || got.MemorySamplerAttempts != 5 || got.MemoryInsufficientAttempts != 0 {
		t.Fatalf("four-poll fallback: %+v", got)
	}
}
func TestUnknownLimitsAndDeadlineKillsSuppressSuggestions(t *testing.T) {
	for _, kind := range []string{"memory", "cpu", "ephemeral", "deadline", "external-deadline"} {
		t.Run(kind, func(t *testing.T) {
			rows := observations()
			for i := range rows {
				rows[i].Usage.Samples = 4
			}
			switch kind {
			case "memory":
				rows[0].MemoryLimitBytes = nil
			case "cpu":
				rows[0].CPULimitNanoCores = nil
			case "ephemeral":
				rows[0].EphemeralLimitBytes = nil
			case "deadline":
				rows[0].DeadlineKilled = true
			case "external-deadline":
				rows[0].Usage.Reason = "DeadlineExceeded"
			}
			got := Build(rows, nil)[0]
			if got.Suggested != nil || len(got.NoSuggestion) == 0 {
				t.Fatalf("uncensored advice: %+v", got)
			}
			deadline := strings.Contains(kind, "deadline")
			if deadline && !got.Flags.DeadlineKilled {
				t.Fatal("deadline flag missing")
			}
			if !deadline && !got.Flags.LimitsUnknown {
				t.Fatal("limits flag missing")
			}
		})
	}
}
func TestStepMemoryAndDeadlineAreBoundToExactIncarnation(t *testing.T) {
	at := time.Now().UTC()
	v := observations()[0].Usage
	v.RunID = strings.Repeat("a", 32)
	v.WorkflowUID = "11111111-1111-4111-8111-111111111111"
	v.PodUID = "22222222-2222-4222-8222-222222222222"
	v.ContainerID = "containerd://" + strings.Repeat("b", 64)
	c := &model.StepTermination{RunID: v.RunID, WorkflowUID: v.WorkflowUID, Namespace: "oberth", WorkflowName: "wf", NodeID: "wf-1", PodName: "wf-1", PodUID: v.PodUID, ContainerName: "main", ContainerID: v.ContainerID, Reason: "Error", PodReason: "DeadlineExceeded", ExitCode: 143, StartedAt: at.Add(-time.Minute), FinishedAt: at}
	v.PodStartedAt = c.StartedAt
	c.ActiveDeadlineSeconds = value(60)
	d := &model.StepDiagnostic{Available: true, ObservedAt: &at, Container: c}
	got := WithStepResult(Observation{Usage: v}, d, value(1536<<20), model.StepTimedOut)
	if got.StepMemoryPeakBytes == nil || *got.StepMemoryPeakBytes != 1536<<20 || !got.DeadlineKilled {
		t.Fatalf("bound peak: %+v", got)
	}
	c.ContainerID = "containerd://" + strings.Repeat("c", 64)
	got = WithStepResult(Observation{Usage: v}, d, value(1536<<20), model.StepTimedOut)
	if got.StepMemoryPeakBytes != nil || got.DeadlineKilled {
		t.Fatal("diagnostic crossed attempt")
	}
}
