package measured

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func value(n int64) *int64 { return &n }
func observations() []Observation {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	var out []Observation
	for i := range 5 {
		v := model.StepResourceUsage{Version: 2, Source: "kubelet-cadvisor", RunID: fmt.Sprint(i), WorkflowName: "wf", PodName: fmt.Sprintf("wf-frag-1234abcd-step-abc12345-%d", i), PodUID: fmt.Sprint(i), ContainerID: fmt.Sprint(i), ContainerName: "main", ContainerKind: "app", PodKind: "workflow", Burn: "step-12345678", Step: "step-abc12345", Terminal: true, Samples: 3, SampledLowerBound: true, LastObservedAt: now, ContainerStartedAt: now.Add(-10 * time.Second), ContainerFinishedAt: now, CPUTimeNS: value(5e9), CPUTimeAt: now, CPUPeakNanoCores: value(1e9), MemoryPeakBytes: value(int64(i+1) * 100 << 20), EphemeralStoragePeakBytes: value(100 << 20)}
		out = append(out, Observation{Usage: v, Template: "frag-1234abcd-step-abc12345", Owner: "repo-01", MemoryLimitBytes: value(1 << 30)})
	}
	return out
}
func TestRunResourcesQuantilesSuggestionsAndStableFragmentIdentity(t *testing.T) {
	rows := observations()
	rows[4].Template = "frag-deadbeef-step-abc12345"
	got := Build(rows, nil)
	if len(got) != 1 || got[0].Template != "step-abc12345" || got[0].OwnerRepository != "repo-01" || got[0].Runs != 5 || got[0].Samples != 15 {
		t.Fatalf("identity: %+v", got)
	}
	s := got[0]
	if *s.MemoryPeakBytes.P50 != 300<<20 || *s.MemoryPeakBytes.P95 != 500<<20 || *s.AverageCPUNanoCores.P95 != 500_000_000 {
		t.Fatalf("stats: %+v", s)
	}
	if s.Suggested == nil || s.Suggested.MemoryLimitBytes < 750<<20 || s.Suggested.CPUMilli != 630 {
		t.Fatalf("suggested: %+v", s.Suggested)
	}
	yaml := YAML(Report{Steps: got})
	if !strings.Contains(yaml, "resources:") || strings.Contains(yaml, "    cpu: 630m\n  requests:") || !strings.Contains(yaml, "activeDeadlineSeconds:") {
		t.Fatal(yaml)
	}
}
func TestRunResourcesWithholdsCensoredAndLowSamples(t *testing.T) {
	rows := observations()
	rows[0].MemoryLimitBytes = value(90 << 20)
	s := Build(rows, nil)[0]
	if s.Suggested != nil || !s.Flags.LimitReached || len(s.NoSuggestion) == 0 {
		t.Fatalf("censored: %+v", s)
	}
	s = Build(rows[1:3], nil)[0]
	if s.Suggested != nil || !s.Flags.LowSamples {
		t.Fatalf("low samples: %+v", s)
	}
}
func TestRunResourcesNeverUsesSelfReportsOrDoublesRepeatedSamples(t *testing.T) {
	rows := observations()
	rows = append(rows, rows...)
	got := Build(rows, nil)[0]
	if got.Runs != 5 || got.Attempts != 5 || got.Samples != 15 {
		t.Fatalf("duplicates: %+v", got)
	}
	rows[0].Usage.Source = "termination"
	rows = rows[:5]
	got = Build(rows, nil)[0]
	if got.Runs != 4 || got.Suggested != nil {
		t.Fatalf("untrusted: %+v", got)
	}
}
func TestRunResourcesMissingCPUAndOOMHaveNoSuggestion(t *testing.T) {
	rows := observations()
	rows[0].Usage.Reason = "OOMKilled"
	rows[0].Usage.RestartCount = 1
	got := Build(rows, nil)[0]
	if !got.Flags.OOM || !got.Flags.Restarts || got.Suggested != nil {
		t.Fatalf("OOM: %+v", got)
	}
	for i := range rows {
		rows[i].Usage.Reason = "Completed"
		rows[i].Usage.CPUTimeNS = nil
		rows[i].Usage.RestartCount = 0
	}
	if s := Build(rows, nil)[0]; s.Suggested != nil || !s.Flags.LowSamples {
		t.Fatalf("missing: %+v", s)
	}
}

func TestRunResourcesAmbiguousIdentityNeverSuggestsEvenWithEnoughMetrics(t *testing.T) {
	rows := observations()
	rows[0].Ambiguous = true
	got := Build(rows, nil)[0]
	if got.Suggested != nil || !got.Flags.IdentityUnknown || got.Flags.LowSamples {
		t.Fatalf("ambiguous identity: %+v", got)
	}
}
