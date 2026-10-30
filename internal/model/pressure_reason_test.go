package model

import "testing"

func TestPressureReasonKeepsDiskFloorFirst(t *testing.T) {
	for _, redacted := range []bool{false, true} {
		blocker := AdmissionBlocker{Size: "M", DiskFloor: true, Pressure: &PressureDeferral{Resource: "io", Avg60: 80, Redacted: redacted}}
		if got := blocker.Reason(); got != "M, held by the disk floor" {
			t.Fatalf("disk floor hidden by pressure (redacted=%t): %s", redacted, got)
		}
		blocker.DiskFloor = false
		if got, want := blocker.Reason(), "M, "+blocker.Pressure.Reason(); got != want {
			t.Fatalf("pressure-only reason = %q, want %q", got, want)
		}
	}
}
