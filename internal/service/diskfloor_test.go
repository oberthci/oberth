package service

import (
	"os"
	"testing"
	"time"
)

func TestDiskFloorDisabled(t *testing.T) {
	check := newDiskFloorCheck("/tmp", 0)
	if !check.freeAboveFloor() {
		t.Fatal("disabled check (floor=0) should always return true")
	}
	if obs := check.observation(); obs != nil {
		t.Fatal("disabled check should return nil observation")
	}
}

func TestDiskFloorNil(t *testing.T) {
	var check *diskFloorCheck
	if !check.freeAboveFloor() {
		t.Fatal("nil check should always return true")
	}
	if obs := check.observation(); obs != nil {
		t.Fatal("nil check should return nil observation")
	}
}

func TestDiskFloorBelowFloor(t *testing.T) {
	// Use a floor so high that any filesystem is below it.
	check := newDiskFloorCheck(os.TempDir(), 1<<62) // 4 EiB
	if check.freeAboveFloor() {
		t.Fatal("no filesystem has 4 EiB free; should report below floor")
	}
	obs := check.observation()
	if obs == nil {
		t.Fatal("should return non-nil observation")
	}
	if !obs.BelowFloor {
		t.Fatal("should report below floor")
	}
	if obs.FreeBytes <= 0 {
		t.Fatal("should report positive free bytes")
	}
}

func TestDiskFloorAboveFloor(t *testing.T) {
	// Use a floor of 1 byte -- any mounted filesystem has at least that.
	check := newDiskFloorCheck(os.TempDir(), 1)
	if !check.freeAboveFloor() {
		t.Fatal("1-byte floor should be above floor on any filesystem")
	}
	obs := check.observation()
	if obs == nil {
		t.Fatal("should return non-nil observation")
	}
	if obs.BelowFloor {
		t.Fatal("should not report below floor")
	}
}

func TestDiskFloorAtFloor(t *testing.T) {
	// Pin one cached observation: two live statfs samples can differ whenever
	// another writer uses the filesystem. Real statfs behavior is covered above.
	const floor int64 = 1024
	for _, test := range []struct {
		name    string
		free    int64
		allowed bool
	}{{"below", floor - 1, false}, {"at", floor, true}, {"above", floor + 1, true}} {
		t.Run(test.name, func(t *testing.T) {
			check := newDiskFloorCheck(os.TempDir(), floor)
			check.cachedFree, check.cacheTime, check.cacheTTL = test.free, time.Now(), time.Hour
			if got := check.freeAboveFloor(); got != test.allowed {
				t.Fatalf("free=%d floor=%d: allowed=%t, want %t", test.free, floor, got, test.allowed)
			}
		})
	}
}

func TestDiskFloorAdmissionHold(t *testing.T) {
	// Verify that the admission gate holds when disk is below floor.
	gate := newWeightedAdmission(10)
	gate.diskFloor = newDiskFloorCheck(os.TempDir(), 1<<62) // impossible floor

	reservation := gate.reserve(1)
	defer reservation.cancel()

	gate.mu.Lock()
	reservation.size = "M"
	reservation.weight = 2
	reservation.prepared = true
	can := gate.canGrantLocked(reservation)
	gate.mu.Unlock()

	if can {
		t.Fatal("admission should be held when disk is below floor")
	}
}

func TestDiskFloorAdmissionGranted(t *testing.T) {
	// Verify that the admission gate grants when disk is above floor.
	gate := newWeightedAdmission(10)
	gate.diskFloor = newDiskFloorCheck(os.TempDir(), 1) // trivial floor

	reservation := gate.reserve(1)
	defer reservation.cancel()

	gate.mu.Lock()
	reservation.size = "M"
	reservation.weight = 2
	reservation.prepared = true
	can := gate.canGrantLocked(reservation)
	gate.mu.Unlock()

	if !can {
		t.Fatal("admission should be granted when disk is above floor")
	}
}
