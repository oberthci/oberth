package stepmodel

import (
	"errors"
	"strconv"
	"testing"
)

func TestCountStepsHasNoDisplayPlanOrVisitCap(t *testing.T) {
	statuses := []StepStatus{StepPending, StepQueued, StepRunning, StepPassed, StepFailed, StepSkipped, StepUnknown}
	for _, total := range []int{0, 1, 2047, 2048, 2049, 65535, 65536, 65537} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			steps := make([]RunStep, total)
			for i := range steps {
				steps[i] = RunStep{Status: statuses[i%len(statuses)], AttemptCount: int(^uint(0) >> 1)}
			}
			for _, exact := range []bool{false, true} {
				assertProjectionCounts(t, steps, exact)
			}
		})
	}
}

func assertProjectionCounts(t *testing.T, steps []RunStep, exact bool) {
	t.Helper()
	got, err := CountSteps(steps, exact)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != len(steps) || got.Exact != exact {
		t.Fatalf("count/exactness = %+v", got)
	}
	buckets := []int{got.Pending, got.Queued, got.Running, got.Passed, got.Failed, got.Skipped, got.Unknown}
	sum := 0
	for i, count := range buckets {
		want := len(steps) / len(buckets)
		if i < len(steps)%len(buckets) {
			want++
		}
		if count != want {
			t.Fatalf("bucket %d = %d, want %d", i, count, want)
		}
		sum += count
	}
	if sum != got.Total {
		t.Fatalf("buckets sum %d differs from total %d", sum, got.Total)
	}
}

func TestInvalidStatusAfterLargePrefixReturnsNoCounts(t *testing.T) {
	steps := make([]RunStep, 65537)
	for i := range steps {
		steps[i].Status = StepPassed
	}
	steps[len(steps)-1] = RunStep{Path: "last", Status: "invalid"}
	got, err := CountSteps(steps, true)
	var invalid *InvalidStatusError
	if !errors.As(err, &invalid) || invalid.Path != "last" || got != (LogicalStepCounts{}) {
		t.Fatalf("got %+v, %v", got, err)
	}
}
