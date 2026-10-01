package service

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/pkg/periapsis"
)

// admissionAcquireBudget bounds an acquire a test expects to succeed, so a
// regression that reintroduces serialization fails with the run number that
// was starved instead of blocking until the package timeout kills the suite.
const admissionAcquireBudget = 2 * time.Second

func acquireWithinBudget(t *testing.T, reservation *admissionReservation, size periapsis.Size) (func(), error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), admissionAcquireBudget)
	defer cancel()
	return reservation.acquire(ctx, size)
}

// Large runs are governed by the weight budget, not by a separate global
// "one at a time" rule. This assertion replaces an earlier one that required
// the second L to block at capacity 6: that rule pinned every L-declaring
// repository to a single concurrent run regardless of maxConcurrentJobs,
// which is the defect this file's newWeightedAdmissionForJobs tests cover.
// Two L runs weigh exactly the capacity here, so both run and a third waits.
func TestWeightedAdmissionEnforcesCapacityForLargeRuns(t *testing.T) {
	gate := newWeightedAdmission(6)
	first := gate.reserve(1)
	releaseFirst, err := first.acquire(context.Background(), periapsis.L)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()

	second := gate.reserve(2)
	releaseSecond, err := acquireWithinBudget(t, second, periapsis.L)
	if err != nil {
		t.Fatalf("two large runs must fit a budget that weighs exactly two of them: %v", err)
	}
	defer releaseSecond()

	third := gate.reserve(3)
	acquired := make(chan func(), 1)
	go acquireForTest(third, periapsis.L, acquired)
	assertNotAcquired(t, acquired)
	releaseSecond()
	releaseThird := awaitAcquired(t, acquired)
	releaseThird()
}

// The regression this file exists for: a run-count ceiling passed straight
// into a weight-denominated budget capped the whole server at one concurrent
// run, because the default M size weighs 2 and the budget was 3. A run claimed
// past that gate sits durably "running" with no Workflow object at all, since
// the gate is acquired before the engine is asked to create one.
func TestWeightedAdmissionForJobsAdmitsConfiguredConcurrencyAtDefaultSize(t *testing.T) {
	const maxConcurrentJobs = 3
	gate := newWeightedAdmissionForJobs(maxConcurrentJobs)
	releases := make([]func(), 0, maxConcurrentJobs)
	for sequence := 1; sequence <= maxConcurrentJobs; sequence++ {
		reservation := gate.reserve(int64(sequence))
		// The empty size is what an undeclared pipeline resolves to.
		release, err := acquireWithinBudget(t, reservation, "")
		if err != nil {
			t.Fatalf("run %d of %d did not acquire at the configured concurrency: %v",
				sequence, maxConcurrentJobs, err)
		}
		releases = append(releases, release)
	}

	overflow := gate.reserve(int64(maxConcurrentJobs) + 1)
	acquired := make(chan func(), 1)
	go acquireForTest(overflow, "", acquired)
	assertNotAcquired(t, acquired)
	releases[0]()
	releaseOverflow := awaitAcquired(t, acquired)
	releaseOverflow()
	for _, release := range releases[1:] {
		release()
	}
}

// A pipeline that declares L trades concurrency for per-run resources
// proportionally. It must not collapse to a single run: that is exactly the
// shape of the reported defect, where every run of one L-declaring repository
// serialized behind the previous one.
func TestWeightedAdmissionForJobsRunsLargePipelinesConcurrently(t *testing.T) {
	gate := newWeightedAdmissionForJobs(3)
	first := gate.reserve(1)
	releaseFirst, err := first.acquire(context.Background(), periapsis.L)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()

	second := gate.reserve(2)
	releaseSecond, err := acquireWithinBudget(t, second, periapsis.L)
	if err != nil {
		t.Fatalf("a second large run must not serialize behind the first: %v", err)
	}
	releaseSecond()
}

// XL declares the whole node and keeps its exclusivity: the budget conversion
// must not weaken the one size that genuinely needs to run alone.
func TestWeightedAdmissionForJobsKeepsExtraLargeExclusive(t *testing.T) {
	gate := newWeightedAdmissionForJobs(3)
	first := gate.reserve(1)
	releaseFirst, err := first.acquire(context.Background(), periapsis.XL)
	if err != nil {
		t.Fatal(err)
	}

	second := gate.reserve(2)
	acquired := make(chan func(), 1)
	go acquireForTest(second, periapsis.S, acquired)
	assertNotAcquired(t, acquired)
	releaseFirst()
	releaseSecond := awaitAcquired(t, acquired)
	releaseSecond()
}

// An operator who configures a single concurrent job still gets one run at the
// default size and heavier. The gate is a resource budget, so a size lighter
// than the default can fit twice in it; the hard run-count ceiling is the
// dispatch loop's `active < maxConcurrent`, not this gate.
func TestWeightedAdmissionForJobsHonorsSingleJobCeiling(t *testing.T) {
	for _, size := range []periapsis.Size{"", periapsis.M, periapsis.L, periapsis.XL} {
		gate := newWeightedAdmissionForJobs(1)
		first := gate.reserve(1)
		releaseFirst, err := first.acquire(context.Background(), size)
		if err != nil {
			t.Fatalf("size %q did not acquire at maxConcurrentJobs=1: %v", size, err)
		}
		second := gate.reserve(2)
		acquired := make(chan func(), 1)
		go acquireForTest(second, size, acquired)
		assertNotAcquired(t, acquired)
		releaseFirst()
		releaseSecond := awaitAcquired(t, acquired)
		releaseSecond()
	}
}

func TestWeightedAdmissionIsFIFOAndXLDrains(t *testing.T) {
	gate := newWeightedAdmission(3)
	medium := gate.reserve(1)
	releaseMedium, err := medium.acquire(context.Background(), periapsis.M)
	if err != nil {
		t.Fatal(err)
	}

	xl := gate.reserve(2)
	small := gate.reserve(3)
	xlAcquired := make(chan func(), 1)
	smallAcquired := make(chan func(), 1)
	go acquireForTest(xl, periapsis.XL, xlAcquired)
	go acquireForTest(small, periapsis.S, smallAcquired)
	assertNotAcquired(t, xlAcquired)
	assertNotAcquired(t, smallAcquired)

	releaseMedium()
	releaseXL := awaitAcquired(t, xlAcquired)
	assertNotAcquired(t, smallAcquired)
	releaseXL()
	releaseSmall := awaitAcquired(t, smallAcquired)
	releaseSmall()
}

func TestWeightedAdmissionReservesClaimOrderBeforeSizingCompletes(t *testing.T) {
	gate := newWeightedAdmission(3)
	first := gate.reserve(1)
	second := gate.reserve(2)
	secondAcquired := make(chan func(), 1)
	go acquireForTest(second, periapsis.S, secondAcquired)
	assertNotAcquired(t, secondAcquired)

	releaseFirst, err := first.acquire(context.Background(), periapsis.XL)
	if err != nil {
		t.Fatal(err)
	}
	assertNotAcquired(t, secondAcquired)
	releaseFirst()
	releaseSecond := awaitAcquired(t, secondAcquired)
	releaseSecond()
}

func TestWeightedAdmissionClampsDefaultMToSmallCapacity(t *testing.T) {
	gate := newWeightedAdmission(1)
	reservation := gate.reserve(1)
	release, err := reservation.acquire(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func acquireForTest(reservation *admissionReservation, size periapsis.Size, acquired chan<- func()) {
	release, err := reservation.acquire(context.Background(), size)
	if err == nil {
		acquired <- release
	}
}

func assertNotAcquired(t *testing.T, acquired <-chan func()) {
	t.Helper()
	select {
	case release := <-acquired:
		release()
		t.Fatal("reservation acquired unexpectedly")
	case <-time.After(20 * time.Millisecond):
	}
}

func awaitAcquired(t *testing.T, acquired <-chan func()) func() {
	t.Helper()
	select {
	case release := <-acquired:
		return release
	case <-time.After(time.Second):
		t.Fatal("reservation did not acquire")
		return nil
	}
}
