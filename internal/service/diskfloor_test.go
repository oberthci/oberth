package service

import (
	"testing"
)

func TestDiskFloorNil(t *testing.T) {
	var check *diskFloorCheck
	if !check.freeAboveFloor() {
		t.Fatal("nil check should always return true")
	}
	if obs := check.observation(); obs != nil {
		t.Fatal("nil check should return nil observation")
	}
}
