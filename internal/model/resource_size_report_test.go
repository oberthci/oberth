package model

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestResourceSizeComparisonUnknownZeroAndCensoredPeaks(t *testing.T) {
	zero, peak, cpu := int64(0), int64(8<<30), int64(2_000_000_000)
	rows := []StepResourceUsage{
		{Burn: "checks", Step: "unit", WorkflowSize: periapsis.XL, Samples: 2, MemoryPeakBytes: &peak, CPUPeakNanoCores: &cpu, Reason: "OOMKilled", Terminal: true},
		{Burn: "checks", Step: "unit", WorkflowSize: periapsis.M, Samples: 1, MemoryPeakBytes: &zero},
		{Burn: "checks", Step: "unit", Samples: 0, Terminal: true}, // legacy class/usage unknown
		{Burn: "checks", Step: "unit", SizeClassChanged: true, Samples: 1, MemoryPeakBytes: &peak},
	}
	before, _ := json.Marshal(rows)
	groups := CompareResourceSize(rows)
	if len(groups) != 3 || groups[0].WorkflowSize != "M" || groups[1].WorkflowSize != "XL" || groups[2].WorkflowSize != "unknown" {
		t.Fatalf("class order/unknowns: %+v", groups)
	}
	if groups[0].MemoryPeakBytes.Count != 1 || *groups[0].MemoryPeakBytes.Max != 0 || groups[0].CPUPeakNanoCores.Count != 0 || groups[0].CPUPeakNanoCores.Max != nil {
		t.Fatal("known zero and unknown were conflated", groups[0])
	}
	if groups[1].OOMKilled != 1 || *groups[1].MemoryPeakBytes.Max != peak || *groups[1].CPUPeakNanoCores.Max != cpu {
		t.Fatal("killed lower-bound peak disappeared", groups[1])
	}
	if groups[2].Observations != 2 || groups[2].Unsampled != 1 || groups[2].Flagged != 1 {
		t.Fatal("coverage flags lost", groups[2])
	}
	steps := []StepResult{{Burn: "checks", Step: "unit", DeclaredSize: periapsis.S}}
	AttachResourceUsage(steps, rows, true)
	if !steps[0].MeasuredUsageTruncated || !reflect.DeepEqual(groups, steps[0].MeasuredSizeComparison) || steps[0].DeclaredSize != periapsis.S {
		t.Fatal("report changed declared metadata or used it as trusted class evidence", steps)
	}
	report := BuildResourceReport(rows)
	if !reflect.DeepEqual(report[0].ByWorkflowSize, groups) {
		t.Fatal("run/repository views disagree")
	}
	after, _ := json.Marshal(rows)
	if string(before) != string(after) {
		t.Fatal("report mutated observations/requests")
	}
}
