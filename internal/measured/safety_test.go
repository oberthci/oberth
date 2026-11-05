package measured

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestHiddenSafetyRowsDoNotChangeQuantilesAndRetainBounds(t *testing.T) {
	rows := observations()
	hidden := rows[0]
	hidden.Usage.RunID = "hidden"
	hidden.Usage.PodUID = "hidden"
	hidden.Usage.MemoryPeakBytes = value(2 << 30)
	hidden.MemoryLimitBytes = value(4 << 30)
	hidden.Usage.CPURequestNanoCores = value(2e9)
	step := BuildWithSafety(rows, nil, []Observation{hidden}, nil)[0]
	if step.Suggested == nil || step.Suggested.MemoryLimitBytes < 4<<30 || step.Suggested.CPUMilli < 2000 || *step.MemoryPeakBytes.Max != 500<<20 || step.Attempts != 5 {
		t.Fatalf("hidden measurements lost floor or contaminated displayed stats: %+v", step)
	}
	hidden.Usage.MemoryPeakBytes = hidden.MemoryLimitBytes
	step = BuildWithSafety(rows, nil, []Observation{hidden}, nil)[0]
	if step.Suggested != nil || step.SizingAcceptance.CensoredAttempts != 1 || !step.Flags.LimitReached {
		t.Fatalf("hidden at-limit sample escaped censoring: %+v", step)
	}
}

func TestKnownCensorDuplicateAndNonterminalLimitAreNotErased(t *testing.T) {
	rows := observations()
	duplicate := rows[0]
	duplicate.DeadlineKilled = true
	step := Build(append(rows, duplicate), nil)[0]
	if step.Suggested != nil || !step.Flags.DeadlineKilled || step.SizingAcceptance.CensoredAttempts != 1 || step.Attempts != 5 {
		t.Fatalf("late duplicate lost or multiplied censor: %+v", step)
	}
	partial := rows[0]
	partial.Usage.RunID = "partial"
	partial.Usage.PodUID = "partial"
	partial.Usage.ContainerID = "partial"
	partial.Usage.Terminal = false
	partial.Usage.ContainerFinishedAt = time.Time{}
	partial.Usage.MemoryPeakBytes = partial.MemoryLimitBytes
	step = Build(append(rows, partial), nil)[0]
	if step.Suggested != nil || !step.Flags.LimitReached || step.SizingAcceptance.CensoredAttempts != 1 {
		t.Fatalf("nonterminal cap evidence disappeared: %+v", step)
	}
}

func TestDuplicatePartialKeepsTerminalMeasurementsAndLimitEvidence(t *testing.T) {
	rows := observations()
	assertCoveredAdvice(t, rows)
	partial := rows[0]
	partial.Usage.Terminal = false
	partial.Usage.ContainerFinishedAt = time.Time{}
	partial.Usage.MemoryPeakBytes = value(1)
	step := Build(append([]Observation{partial}, rows...), nil)[0]
	if step.Suggested == nil || step.Attempts != 5 || step.MemoryPeakBytes.Count != 5 {
		t.Fatalf("partial duplicate displaced completed measurements: %+v", step)
	}
	partial.Usage.MemoryPeakBytes = partial.MemoryLimitBytes
	step = Build(append(rows, partial), nil)[0]
	if step.Suggested != nil || !step.Flags.LimitReached || step.SizingAcceptance.CensoredAttempts != 1 || step.MemoryPeakBytes.Count != 5 {
		t.Fatalf("late duplicate erased cap evidence or changed quantiles: %+v", step)
	}
}

func TestIndependentPodDispositionWithoutUsageHasNoInventedNumbers(t *testing.T) {
	d := model.PodResourceDisposition{RunID: strings.Repeat("a", 32), WorkflowName: "wf", WorkflowUID: "11111111-1111-4111-8111-111111111111", Namespace: "argo", NodeID: "wf-1", PodName: "wf-1", PodUID: "22222222-2222-4222-8222-222222222222", Stage: "step-12345678", Step: "step-abc12345", Template: "step-abc12345", Owner: "repo-01", Reason: "Evicted", ObservedAt: time.Now().UTC()}
	for _, reason := range []string{"Evicted", "Completed", "Failed"} {
		d.Reason = reason
		step := BuildWithSafety(nil, nil, nil, []model.PodResourceDisposition{d})[0]
		if step.Suggested != nil || step.Attempts != 0 || step.Samples != 0 || step.MemoryPeakBytes.Count != 0 || step.DurationNS.Count != 0 || len(step.NoSuggestion) == 0 {
			t.Fatalf("no-row disposition became measurements: %+v", step)
		}
		if step.Flags.IdentityUnknown {
			t.Fatal("known disposition provenance became unknown")
		}
		if reason == "Evicted" && step.SizingAcceptance.Status != "censored" {
			t.Fatal("eviction was not retained as censoring")
		}
	}
	rows := observations()
	d.Reason = "Evicted"
	d.Template = "unrelated"
	for _, step := range BuildWithSafety(rows, nil, nil, []model.PodResourceDisposition{d}) {
		if step.Template == "step-abc12345" && step.Suggested == nil {
			t.Fatal("unrelated invocation poisoned advice")
		}
	}
}

func TestUnknownBaselineCannotTurnUnlimitedIntoFiniteLimits(t *testing.T) {
	for _, kind := range []string{"unknown-request", "unlimited-memory", "unlimited-storage", "mixed-storage"} {
		rows := observations()
		assertCoveredAdvice(t, rows)
		for i := range rows {
			switch kind {
			case "unknown-request":
				rows[i].Usage.MemoryRequestBytes = nil
			case "unlimited-memory":
				rows[i].MemoryLimitBytes = value(0)
			case "unlimited-storage":
				rows[i].EphemeralLimitBytes = value(0)
				rows[i].Usage.PodEphemeralLimitBytes = value(0)
			case "mixed-storage":
				rows[i].Usage.PodEphemeralLimitBytes = value(1 << 30)
			}
		}
		step := Build(rows, nil)[0]
		if step.Suggested != nil || !strings.Contains(strings.Join(step.NoSuggestion, " "), "baseline") {
			t.Fatalf("%s manufactured a restrictive limit: %+v", kind, step)
		}
	}
}

func TestBoundDiagnosticOOMSurvivesPartialTelemetryButNotWrongRetry(t *testing.T) {
	rows := observations()
	assertCoveredAdvice(t, rows)
	partial := rows[0]
	v := &partial.Usage
	v.RunID = strings.Repeat("a", 32)
	v.WorkflowUID = "11111111-1111-4111-8111-111111111111"
	v.PodUID = "22222222-2222-4222-8222-222222222222"
	v.ContainerID = "containerd://" + strings.Repeat("b", 64)
	at := v.ContainerFinishedAt
	v.Terminal = false
	v.ExitCode = nil
	v.Reason = ""
	v.ContainerFinishedAt = time.Time{}
	c := &model.StepTermination{RunID: v.RunID, Namespace: "argo", WorkflowName: "wf", WorkflowUID: v.WorkflowUID, NodeID: "wf-1", PodName: "wf-1", PodUID: v.PodUID, ContainerName: "main", ContainerID: v.ContainerID, Reason: "OOMKilled", PodReason: "none", ExitCode: 137, StartedAt: v.ContainerStartedAt, FinishedAt: at}
	d := &model.StepDiagnostic{Available: true, ObservedAt: &at, Container: c}
	if d.Validate() != nil {
		t.Fatal("invalid diagnostic fixture")
	}
	bound := WithStepResult(partial, d, nil, model.StepFailed)
	step := Build(append(rows, bound), nil)[0]
	if step.Suggested != nil || !step.Flags.OOM || step.SizingAcceptance.CensoredAttempts != 1 || step.MemoryPeakBytes.Count != 5 {
		t.Fatalf("bound OOM lost or partial metrics entered quantiles: %+v", step)
	}
	c.ContainerID = "containerd://" + strings.Repeat("c", 64)
	wrong := WithStepResult(partial, d, nil, model.StepFailed)
	step = Build(append(rows, wrong), nil)[0]
	if step.Suggested == nil || step.Flags.OOM {
		t.Fatal("diagnostic crossed runtime incarnation")
	}
}
