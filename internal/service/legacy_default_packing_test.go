package service

import "testing"

func TestDefaultLegacyChargesBoundSmallRunPacking(t *testing.T) {
	gate := newWeightedAdmission(20)
	for sequence := int64(1); sequence <= 8; sequence++ {
		release := acquireRequestWithinBudget(t, gate.reserve(sequence), admissionRequest{Size: "S"})
		t.Cleanup(release)
	}
	extra := pending(t, gate, 9, admissionRequest{Size: "S"})
	t.Cleanup(extra.cancel)
	if granted(gate, extra) {
		t.Fatal("ninth S run crossed the default numeric budget despite spare weight")
	}
	state := (&Scheduler{admission: gate}).AdmissionState()
	if state.Runs != 8 || state.Used != 8 || state.Capacity != 20 || state.NumericBudget == nil || state.NumericBudget.CPUUsedMilli != 8000 || state.NumericBudget.MemoryUsedBytes != 16<<30 {
		t.Fatalf("small-run numeric limit or spare weight changed: %+v", state)
	}
}
