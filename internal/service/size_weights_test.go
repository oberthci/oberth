package service

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestParseSizeWeightsValidates(t *testing.T) {
	if weights, err := ParseSizeWeights(""); err != nil || weights != DefaultSizeWeights {
		t.Fatalf("empty = %+v, %v; want the defaults", weights, err)
	}
	if weights, err := ParseSizeWeights("XL=11"); err != nil || weights != (SizeWeights{S: 1, M: 2, L: 3, XL: 11}) {
		t.Fatalf("XL=11 = %+v, %v", weights, err)
	}
	if weights, err := ParseSizeWeights(DefaultSizeWeights.String()); err != nil || weights != DefaultSizeWeights {
		t.Fatalf("round trip = %+v, %v", weights, err)
	}
	for value, expect := range map[string]string{
		"S=0":      "S=0 must be at least 1",
		"M=0":      "M=0 must be at least 1",
		"L=0":      "L=0 must be at least 1",
		"XL=-1":    "XL=-1 must be 0",
		"S=3":      "S <= M <= L",
		"M=4":      "S <= M <= L",
		"XL=2":     "L <= XL",
		"XXL=4":    "unknown size",
		"S=1,S=2":  "more than once",
		"S=one":    "not an integer",
		"S":        "not SIZE=WEIGHT",
		"S=1,,M=2": "not SIZE=WEIGHT",
	} {
		if _, err := ParseSizeWeights(value); err == nil || !strings.Contains(err.Error(), expect) {
			t.Fatalf("%q: %v, want %q", value, err, expect)
		}
	}
	if _, err := NewScheduler(SchedulerConfig{SizeWeights: SizeWeights{S: 2, M: 1, L: 3}}); err == nil {
		t.Fatal("NewScheduler accepted invalid weights")
	}
}

// Size weights remain parseable for upgrade compatibility, but neither a
// whole-budget nor a finite XL weight may change numeric admission.
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
