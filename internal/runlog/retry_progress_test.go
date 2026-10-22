package runlog

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/runprogress"
)

func TestLegacyRetryOrphansDoNotOwnJournalOrdinals(t *testing.T) {
	for _, canonicalSource := range []string{"plan", "journal"} {
		t.Run(canonicalSource, func(t *testing.T) {
			logs, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if canonicalSource == "plan" {
				if err := logs.WriteStepPlan("run", []runprogress.PlannedStep{{Burn: "x", Step: "step", Ordinal: 1}}); err != nil {
					t.Fatal(err)
				}
			}
			events := []runprogress.Event{
				{Version: 1, Burn: "x(0)", Step: "step", Ordinal: 0, Status: runprogress.StepQueued},
				{Version: 1, Burn: "parallel", Step: "step", Ordinal: 0, Status: runprogress.StepQueued},
			}
			if canonicalSource == "journal" {
				events = append(events, runprogress.Event{Version: 1, Burn: "x", Step: "step", Ordinal: 1, Status: runprogress.StepQueued})
			}
			for _, event := range events {
				if err := logs.AppendStepProgress("run", event); err != nil {
					t.Fatal(err)
				}
			}
			progress, err := logs.StepProgress("run")
			if err != nil || len(progress) != len(events)-1 {
				t.Fatalf("legacy progress: %+v: %v", progress, err)
			}
			file, resumed, _, err := logs.Resume("run")
			if err != nil {
				t.Fatalf("legacy resume: %v", err)
			}
			_ = file.Close()
			for _, event := range resumed {
				if event.Burn == "x(0)" {
					t.Fatalf("legacy orphan restored: %+v", event)
				}
			}
		})
	}
}

func TestRetryCompatibilityStillRejectsGenuineOrdinalConflicts(t *testing.T) {
	for _, conflict := range []string{"changed", "shared", "planned-suffix"} {
		t.Run(conflict, func(t *testing.T) {
			logs, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			orphan := runprogress.Event{Version: 1, Burn: "x(0)", Step: "step", Ordinal: 0, Status: runprogress.StepQueued}
			canonical := orphan
			canonical.Burn, canonical.Ordinal = "x", 1
			bad := canonical
			want := "ordinal changed"
			switch conflict {
			case "changed":
				bad.Ordinal = 2
			case "shared":
				bad.Burn = "parallel"
				want = "ordinal is shared"
			case "planned-suffix":
				if err := logs.WriteStepPlan("run", []runprogress.PlannedStep{
					{Burn: "x(0)", Step: "step", Ordinal: 0}, {Burn: "x", Step: "step", Ordinal: 1},
				}); err != nil {
					t.Fatal(err)
				}
				bad = orphan
				bad.Ordinal = 2
			}
			for _, event := range []runprogress.Event{orphan, canonical, bad} {
				if err := logs.AppendStepProgress("run", event); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := logs.StepProgress("run"); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("genuine conflict not rejected: %v", err)
			}
			if file, _, _, err := logs.Resume("run"); err == nil {
				_ = file.Close()
				t.Fatal("resume accepted a genuine conflict")
			}
		})
	}
}
