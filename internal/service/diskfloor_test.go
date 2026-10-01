package service

import (
	"os"
	"testing"
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
	// Read the actual free bytes and set the floor exactly there.
	free := statfsFreeBytes(os.TempDir())
	if free <= 0 {
		t.Skip("cannot determine free bytes on this filesystem")
	}
	check := newDiskFloorCheck(os.TempDir(), free)
	// At exactly the floor, freeAboveFloor returns true (>= comparison).
	if !check.freeAboveFloor() {
		t.Fatal("at-floor should be accepted (>= comparison)")
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
