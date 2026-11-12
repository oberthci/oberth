package measured

import (
	"fmt"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/model"
)

func cpuHistoryRow(run string, milli int64) model.StepResourceUsage {
	start := time.Unix(1000, 0)
	end := start.Add(100 * time.Second)
	return model.StepResourceUsage{RunID: run, TemplateName: "main", ContainerName: "main", Terminal: true, ExitCode: ptr.To(int32(0)), CPUTimeNS: ptr.To(milli * 100_000_000), CPUTimeAt: end, ContainerStartedAt: start, ContainerFinishedAt: end, CPUCoverage: model.MetricCoverage{Count: 4, FirstAt: start, LastAt: end}, CPUPeakNanoCores: ptr.To(int64(8_000_000_000))}
}
func TestCPURequestsUseP95OfRunAveragesNotPeaks(t *testing.T) {
	var rows []model.StepResourceUsage
	for i := range 20 {
		milli := int64(10)
		if i == 19 {
			milli = 3000
		}
		rows = append(rows, cpuHistoryRow(fmt.Sprint(i), milli))
	}
	if got := CPURequestPercentile(rows)["main"]; got != 50 {
		t.Fatalf("P95 of 19 small runs and one outlier: got %dm, want 50m; peaks must not size requests", got)
	}
	rows = nil
	for i := range 5 {
		rows = append(rows, cpuHistoryRow(fmt.Sprint(i), 200))
	}
	if got := CPURequestPercentile(rows)["main"]; got != 250 {
		t.Fatalf("P95+25%%: %dm", got)
	}
}
func TestCPURequestHistoryNeedsDistinctCompleteSuccessfulRuns(t *testing.T) {
	for _, change := range []string{"duplicate", "short", "failed", "reset", "saturated", "restart", "unknown-template", "changed", "at-cpu-limit"} {
		t.Run(change, func(t *testing.T) {
			var rows []model.StepResourceUsage
			for i := range 5 {
				v := cpuHistoryRow(fmt.Sprint(i), 10)
				switch change {
				case "duplicate":
					v.RunID = "one"
				case "short":
					v.CPUCoverage.LastAt = v.ContainerFinishedAt.Add(-time.Second)
				case "failed":
					v.ExitCode = ptr.To(int32(1))
				case "reset":
					v.CounterReset = true
				case "saturated":
					v.Saturated = true
				case "restart":
					v.RestartCount = 1
				case "unknown-template":
					v.TemplateName = ""
				case "at-cpu-limit":
					v.CPULimitNanoCores = ptr.To(int64(8_000_000_000))
				case "changed":
					v.RequestsChanged = true
				}
				rows = append(rows, v)
			}
			if len(CPURequestPercentile(rows)) != 0 {
				t.Fatal("incomplete/failed/duplicate samples authorized sizing")
			}
		})
	}
}
