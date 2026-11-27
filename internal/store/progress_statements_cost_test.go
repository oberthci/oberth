package store

import (
	"syscall"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func BenchmarkNativeProgressPrepared(b *testing.B) {
	s := benchmarkStore(b)
	id := seedRunView(b, s, 1)
	statuses := []stepmodel.StepStatus{stepmodel.StepRunning, stepmodel.StepQueued}
	i := 0
	cpu := progressBenchmarkCPU(b)
	b.ReportAllocs()
	for b.Loop() {
		i++
		if err := s.SaveProgress(b.Context(), id, stateport.Progress{Steps: []stepmodel.RunStep{{Path: "step-00", Name: "step-00", Status: statuses[i%2]}}}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric((progressBenchmarkCPU(b)-cpu)/float64(b.N), "cpu-ns/op")
}
func progressBenchmarkCPU(b *testing.B) float64 {
	b.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		b.Fatal(err)
	}
	return float64(usage.Utime.Sec+usage.Stime.Sec)*1e9 + float64(usage.Utime.Usec+usage.Stime.Usec)*1e3
}
