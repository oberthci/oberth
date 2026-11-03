package measured

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func acceptance(t *testing.T, step Step) map[string]any {
	t.Helper()
	body, err := json.Marshal(step)
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(body, &row); err != nil {
		t.Fatal(err)
	}
	result, ok := row["sizing_acceptance"].(map[string]any)
	if !ok {
		t.Fatalf("missing per-step sizing acceptance: %s", body)
	}
	return result
}

func TestSizingAcceptanceRequiresJointCompleteComparableTerminalRuns(t *testing.T) {
	for _, test := range []struct {
		name     string
		change   func([]Observation) []Observation
		status   string
		complete int
	}{
		{"complete", func(v []Observation) []Observation { return v }, "sufficient", 5},
		{"too few", func(v []Observation) []Observation { return v[:4] }, "insufficient", 4},
		{"nonterminal", func(v []Observation) []Observation { v[0].Usage.Terminal = false; return v }, "insufficient", 4},
		{"missing CPU peak", func(v []Observation) []Observation { v[0].Usage.CPUPeakNanoCores = nil; return v }, "insufficient", 4},
		{"unknown limits", func(v []Observation) []Observation { v[0].MemoryLimitBytes = nil; return v }, "insufficient", 4},
		{"changed limits", func(v []Observation) []Observation { v[0].MemoryLimitBytes = value(2 << 30); return v }, "non_comparable", 4},
		{"changed requests", func(v []Observation) []Observation { v[0].Usage.CPURequestNanoCores = value(1e9); return v }, "non_comparable", 4},
		{"censored", func(v []Observation) []Observation { v[0].Usage.MemoryPeakBytes = value(1 << 30); return v }, "censored", 4},
		{"oom", func(v []Observation) []Observation { v[0].Usage.Reason = "OOMKilled"; return v }, "censored", 4},
		{"deadline", func(v []Observation) []Observation { v[0].DeadlineKilled = true; return v }, "censored", 4},
		{"retries are not runs", func(v []Observation) []Observation {
			for i := range v {
				v[i].Usage.RunID = "same"
			}
			return v
		}, "insufficient", 1},
		{"metrics from different runs", func(v []Observation) []Observation {
			for i := 5; i < 7; i++ {
				row := v[0]
				row.Usage.RunID = fmt.Sprint(i)
				row.Usage.PodUID = fmt.Sprint(i)
				v = append(v, row)
			}
			v[0].Usage.CPUTimeNS = nil
			v[1].Usage.EphemeralStoragePeakBytes = nil
			v[2].Usage.ContainerFinishedAt = time.Time{}
			return v
		}, "insufficient", 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			step := Build(test.change(observations()), nil)[0]
			if test.status != "sufficient" && step.Suggested != nil {
				t.Fatalf("unsafe suggestion: %+v", step)
			}
			got := acceptance(t, step)
			if got["status"] != test.status || got["comparable_complete_runs"] != float64(test.complete) || got["required_runs"] != float64(5) {
				t.Fatalf("acceptance: %+v", got)
			}
			if got["sufficient"] != (test.status == "sufficient") {
				t.Fatalf("sufficient: %+v", got)
			}
			if test.status == "censored" && got["censored_attempts"] != float64(1) {
				t.Fatalf("censoring: %+v", got)
			}
		})
	}
}

func TestSizingAcceptanceIsVisibleInYAMLAndMCP(t *testing.T) {
	report := Report{Steps: Build(observations()[:4], nil)}
	for _, format := range []string{"json", "yaml"} {
		report.Format = format
		text := report.MCPToolText()
		if !strings.Contains(text, "sizing_acceptance") || !strings.Contains(text, "insufficient") {
			t.Fatal(text)
		}
	}
}

func TestSizingAcceptanceRefusesTruncatedHistory(t *testing.T) {
	step := Build(observations(), nil)[0]
	step.RefuseSizing("history row limit reached")
	if step.Suggested != nil || step.SizingAcceptance.Sufficient || step.SizingAcceptance.Status != "insufficient" || len(step.SizingAcceptance.Reasons) != 1 {
		t.Fatalf("truncated history: %+v", step)
	}
}
