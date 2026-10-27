package app

import (
	"testing"
	"time"

	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stepusage"
)

func TestArgoResultPreservesMeasuredAndUnknownUsage(t *testing.T) {
	jobs := &ArgoJobs{}
	result := jobs.result(argojob.Completion{Succeeded: true, Steps: []argojob.StepResult{
		{Burn: "test", Step: "measured", Usage: stepusage.Usage{MaxRSSBytes: ptr.To(int64(65536)), MemoryPeakBytes: ptr.To(int64(131072)), UserCPU: ptr.To(time.Second), SystemCPU: ptr.To(time.Duration(0))}},
		{Burn: "test", Step: "unknown"},
	}})
	known, unknown := result.Steps[0], result.Steps[1]
	if known.MaxRSSBytes == nil || *known.MaxRSSBytes != 65536 || known.MemoryPeakBytes == nil || *known.MemoryPeakBytes != 131072 || known.UserCPU == nil || *known.UserCPU != time.Second || known.SystemCPU == nil || *known.SystemCPU != 0 {
		t.Fatalf("mapping lost usage: %+v", known)
	}
	if unknown.MaxRSSBytes != nil || unknown.MemoryPeakBytes != nil || unknown.UserCPU != nil || unknown.SystemCPU != nil {
		t.Fatalf("unknown mapped to zero: %+v", unknown)
	}
}
