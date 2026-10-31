package model

import (
	"math"
	"testing"
	"time"
)

func TestResourceQuantilesMissingZeroAndPairedRatios(t *testing.T) {
	p := func(v int64) *int64 { return &v }
	rows := []StepResourceUsage{{Burn: "checks", Step: "test", MemoryPeakBytes: p(0), MemoryRequestBytes: p(100), Samples: 1}, {Burn: "checks", Step: "test", MemoryPeakBytes: p(100), MemoryRequestBytes: p(200), Samples: 1, Terminal: true, Reason: "OOMKilled"}, {Burn: "checks", Step: "test", MemoryRequestBytes: p(0), Samples: 0, Terminal: true}}
	r := BuildResourceReport(rows)
	m := r[0].Metrics["memory_bytes"]
	if len(r) != 1 || r[0].Observations != 3 || r[0].Unsampled != 1 || r[0].Killed != 1 || m.Used.Count != 2 || *m.Used.P50 != 0 || *m.Used.P95 != 100 || *m.Used.Max != 100 || m.UsedRequestPPM.Count != 2 || *m.UsedRequestPPM.Max != 500000 || m.Requested.Count != 3 {
		t.Fatalf("report %#v %#v", r, m)
	}
	if m := r[0].Metrics["io_read_bytes"]; m.Used.Count != 0 || m.Used.P50 != nil {
		t.Fatal("missing became zero")
	}
}
func TestResourceRateOverflowAndQuantileRanks(t *testing.T) {
	if v, sat := ResourceRate(math.MaxInt64, time.Second); v != math.MaxInt64 || sat {
		t.Fatal(v, sat)
	}
	if v, sat := ResourceRate(math.MaxInt64, time.Nanosecond); v != math.MaxInt64 || !sat {
		t.Fatal(v, sat)
	}
	q := Quantiles([]int64{5, 1, 3, 2, 4})
	if *q.P50 != 3 || *q.P95 != 5 || *q.Max != 5 {
		t.Fatal(q)
	}
}
