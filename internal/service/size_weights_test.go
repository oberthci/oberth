package service

import (
	"testing"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestSchedulerRejectsInvalidSizeWeights(t *testing.T) {
	if _, err := NewScheduler(SchedulerConfig{SizeWeights: SizeWeights{S: 2, M: 1, L: 3}}); err == nil {
		t.Fatal("NewScheduler accepted invalid weights")
	}
}

// Deprecated size weights must not change workflow admission, whether XL
// historically consumed the whole budget or a finite weight.
func TestDeprecatedWeightsDoNotChangeNumericAdmission(t *testing.T) {
	for _, weights := range []SizeWeights{DefaultSizeWeights, {S: 1, M: 2, L: 3, XL: 11}} {
		gate := newWorkflowAdmissionForJobs(8)
		gate.weights = weights
		if gate.capacity != 8 || gate.maxRuns != 8 {
			t.Fatal("run ceiling changed by weights", gate.capacity, gate.maxRuns)
		}
		var releases []func()
		for i := int64(1); i <= 8; i++ {
			releases = append(releases, acquireRequestWithinBudget(t, gate.reserve(i), admissionRequest{Size: pipeline.XL}))
		}
		budget := gate.numericBudgetLocked()
		if gate.used != 0 || gate.runs != 8 || budget != nil {
			t.Fatalf("numeric ledger = %+v, runs %d weights %d", budget, gate.runs, gate.used)
		}
		for _, release := range releases {
			release()
		}
	}
}

// With S heavier than 1 the dispatch loop must not claim into spare weight
// no S run can use: 15 of 16 used leaves no room for an S of weight 2.
func TestSpareClaimUsesRunCeilingRegardlessOfSizeWeights(t *testing.T) {
	gate := newWorkflowAdmissionForJobs(4)
	for i := int64(1); i <= 3; i++ {
		acquireRequestWithinBudget(t, gate.reserve(i), admissionRequest{Size: pipeline.L})
	}
	if !gate.spareWeightForClaim(3) {
		t.Fatal("size weights prevented claiming into a free numeric slot")
	}
	acquireRequestWithinBudget(t, gate.reserve(4), admissionRequest{Size: pipeline.XL})
	if gate.spareWeightForClaim(4) {
		t.Fatal("claimed past admin run ceiling")
	}
}
