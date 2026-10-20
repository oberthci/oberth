package service

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/periapsis"
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

// With the defaults the gate behaves exactly as before: XL is the whole budget.
func TestDefaultWeightsKeepXLExclusive(t *testing.T) {
	gate := newWeightedAdmissionForJobsWeights(8, DefaultSizeWeights)
	if gate.capacity != 16 {
		t.Fatalf("capacity = %d, want 16", gate.capacity)
	}
	release := acquireRequestWithinBudget(t, gate.reserve(1), admissionRequest{RunID: "xl", Size: periapsis.XL})
	defer release()
	if used, _ := gateUsage(gate); used != 16 {
		t.Fatalf("XL used %d, want the whole budget 16", used)
	}
	beside := gate.reserve(2)
	ready := make(chan func(), 1)
	go acquireRequestForTest(beside, admissionRequest{RunID: "s", Size: periapsis.S}, ready)
	awaitWaiting(t, beside)
	if blocker := (&Scheduler{admission: gate}).AdmissionBlocker(); beside.granted || blocker == nil || !blocker.FullDrain && blocker.RunID != "s" {
		t.Fatalf("an S run was admitted beside a whole-budget XL: %#v", blocker)
	}
	beside.cancel()
}

// XL=11 of a 16 budget shares the host: an L (3) fits beside it, a second L
// would not (17), and the queue head names the configured weight.
func TestFiniteXLWeightSharesTheHost(t *testing.T) {
	weights := SizeWeights{S: 1, M: 2, L: 3, XL: 11}
	gate := newWeightedAdmissionForJobsWeights(8, weights)
	scheduler := &Scheduler{admission: gate}
	releaseXL := acquireRequestWithinBudget(t, gate.reserve(1), admissionRequest{RunID: "xl", Repo: "ebpf", Ref: "main", Size: periapsis.XL})
	defer releaseXL()
	releaseL := acquireRequestWithinBudget(t, gate.reserve(2), admissionRequest{RunID: "l", Size: periapsis.L})
	defer releaseL()
	if used, runs := gateUsage(gate); used != 14 || runs != 2 {
		t.Fatalf("used %d runs %d, want 14 and 2", used, runs)
	}
	second := gate.reserve(3)
	ready := make(chan func(), 1)
	go acquireRequestForTest(second, admissionRequest{RunID: "l2", Repo: "cli", Ref: "feature/x", Size: periapsis.L}, ready)
	awaitWaiting(t, second)
	if blocker := scheduler.AdmissionBlocker(); blocker == nil || blocker.FullDrain || blocker.Reason() != "L, needs 3 weight; 14/16 used" {
		t.Fatalf("blocker = %#v", blocker)
	}
	releaseL()
	awaitAcquired(t, ready)()

	// An XL waiting for weight says its configured weight, not a drain.
	head := newWeightedAdmissionForJobsWeights(8, weights)
	hold := acquireRequestWithinBudget(t, head.reserve(1), admissionRequest{RunID: "m", Size: periapsis.L})
	defer hold()
	for index := int64(2); index <= 3; index++ {
		acquireRequestWithinBudget(t, head.reserve(index), admissionRequest{RunID: "m", Size: periapsis.M})
	}
	xl := head.reserve(4)
	go acquireRequestForTest(xl, admissionRequest{RunID: "xl", Repo: "ebpf", Ref: "main", Size: periapsis.XL}, make(chan func(), 1))
	awaitWaiting(t, xl)
	blocker := (&Scheduler{admission: head}).AdmissionBlocker()
	if blocker == nil || blocker.FullDrain || !strings.HasPrefix(blocker.Reason(), "XL · 11w") {
		t.Fatalf("finite XL blocker = %#v (%s)", blocker, blocker.Reason())
	}
	xl.cancel()
}

// With S heavier than 1 the dispatch loop must not claim into spare weight
// no S run can use: 15 of 16 used leaves no room for an S of weight 2.
func TestSpareClaimNeedsRoomForAnSRun(t *testing.T) {
	gate := newWeightedAdmissionForJobsWeights(4, SizeWeights{S: 2, M: 4, L: 5, XL: 0}) // budget 16
	for index, size := range []periapsis.Size{periapsis.L, periapsis.L, periapsis.L} {
		acquireRequestWithinBudget(t, gate.reserve(int64(index+1)), admissionRequest{RunID: "l", Size: size})
	}
	if used, runs := gateUsage(gate); used != 15 || runs != 3 {
		t.Fatalf("used %d runs %d", used, runs)
	}
	if gate.spareWeightForClaim(3) {
		t.Fatal("claimed into 1 spare weight with S weighing 2")
	}
	gate.weights.S = 1
	if !gate.spareWeightForClaim(3) {
		t.Fatal("did not claim into spare weight an S run can use")
	}
}
