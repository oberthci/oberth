package measured

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

func TestMainRuntimeCannotSupplyPodDeadline(t *testing.T) {
	for _, podTiming := range []string{"missing", "init-and-wait", "last-exit-before-finalization"} {
		t.Run(podTiming, func(t *testing.T) {
			rows := observations()
			for i := range rows {
				v := &rows[i].Usage
				switch podTiming {
				case "init-and-wait":
					v.PodStartedAt = v.ContainerStartedAt.Add(-5 * time.Minute)
					v.PodFinishedAt = v.ContainerFinishedAt.Add(5 * time.Minute)
					v.StorageCoverage.LastAt = v.PodFinishedAt
					v.LastObservedAt = v.PodFinishedAt
				case "last-exit-before-finalization":
					// Even a retained terminal Pod finish is the last container
					// exit, not the time Pod finalization completed.
					v.PodStartedAt = v.ContainerStartedAt
					v.PodFinishedAt = v.ContainerFinishedAt
				}
			}
			declaration := Declaration{Stage: "step-12345678", Template: "step-abc12345", ActiveDeadlineSeconds: value(600)}
			step := Build(rows, []Declaration{declaration})[0]
			if step.Suggested == nil || !step.SizingAcceptance.Sufficient || step.Suggested.CPUMilli != 630 || step.Suggested.MemoryLimitBytes != 1<<30 || *step.DurationNS.Max != int64(10*time.Second) || *step.AverageCPUNanoCores.P95 != 500_000_000 {
				t.Fatalf("resource advice or main runtime changed: %+v", step)
			}
			assertNoDeadlineAdvice(t, step, true)
		})
	}
}

func TestDeadlineAdviceNoDataAndCensoredHistory(t *testing.T) {
	declaration := Declaration{Stage: "step-12345678", Template: "step-abc12345", ActiveDeadlineSeconds: value(600)}
	for _, kind := range []string{"no-data", "missing-main-finish", "deadline", "oom", "limit"} {
		t.Run(kind, func(t *testing.T) {
			rows := observations()
			switch kind {
			case "no-data":
				rows = nil
			case "missing-main-finish":
				for i := range rows {
					rows[i].Usage.ContainerFinishedAt = time.Time{}
				}
			case "deadline":
				rows[0].DeadlineKilled = true
			case "oom":
				rows[0].Usage.Reason = "OOMKilled"
			case "limit":
				rows[0].MemoryLimitBytes = rows[0].Usage.MemoryPeakBytes
			}
			step := Build(rows, []Declaration{declaration})[0]
			if step.Suggested != nil || step.Declared == nil || *step.Declared.ActiveDeadlineSeconds != 600 || len(step.NoSuggestion) == 0 {
				t.Fatalf("missing/censored data or declared deadline changed: %+v", step)
			}
			if (kind == "no-data" || kind == "missing-main-finish") && step.DurationNS.Count != 0 {
				t.Fatalf("missing runtime became a measurement: %+v", step.DurationNS)
			}
			if (kind == "deadline" || kind == "oom" || kind == "limit") && step.SizingAcceptance.Status != "censored" {
				t.Fatalf("censoring lost: %+v", step.SizingAcceptance)
			}
			assertNoDeadlineAdvice(t, step, false)
		})
	}
}

func assertNoDeadlineAdvice(t *testing.T, step Step, wantResources bool) {
	t.Helper()
	for _, format := range []string{"json", "yaml"} {
		report := Report{Format: format, Steps: []Step{step}}
		body := []byte(report.MCPToolText())
		if format == "yaml" {
			var err error
			body, err = yaml.YAMLToJSON(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		var document struct {
			Steps []map[string]json.RawMessage `json:"steps"`
		}
		if err := json.Unmarshal(body, &document); err != nil {
			t.Fatal(err)
		}
		row := document.Steps[0]
		for _, name := range []string{"suggested", "paste_ready"} {
			var block map[string]json.RawMessage
			if raw := row[name]; raw != nil {
				if err := json.Unmarshal(raw, &block); err != nil {
					t.Fatal(err)
				}
			}
			if _, exists := block["activeDeadlineSeconds"]; exists {
				t.Fatalf("%s %s invents a Pod deadline: %s", format, name, body)
			}
			if wantResources && (name == "suggested" || format == "yaml") && len(block) == 0 {
				t.Fatalf("%s %s lost resource advice: %s", format, name, body)
			}
		}
		var reason string
		if err := json.Unmarshal(row["deadline_no_suggestion"], &reason); err != nil || !strings.Contains(reason, "finalization") || !strings.Contains(reason, "main-container") {
			t.Fatalf("%s missing timing explanation: %s", format, body)
		}
	}
}
