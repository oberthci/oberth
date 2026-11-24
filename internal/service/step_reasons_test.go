package service_test

import (
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

func TestStepReadKeepsDiagnosticsWithoutInventingStatusSentences(t *testing.T) {
	for _, status := range []stepmodel.StepStatus{stepmodel.StepPending, stepmodel.StepQueued, stepmodel.StepRunning, stepmodel.StepPassed, stepmodel.StepFailed, stepmodel.StepSkipped, stepmodel.StepUnknown} {
		fallback := ""
		if status == stepmodel.StepUnknown {
			fallback = "execution evidence unavailable"
		}
		for _, tc := range []struct{ reason, want string }{{"", fallback}, {string(status), fallback}, {"step " + string(status), fallback}, {"waiting for a node", "waiting for a node"}, {"step passed with warnings", "step passed with warnings"}} {
			t.Run(string(status)+"/"+tc.reason, func(t *testing.T) { requireStepDiagnostic(t, status, tc.reason, tc.want) })
		}
	}
}

func requireStepDiagnostic(t *testing.T, status stepmodel.StepStatus, reason, want string) {
	t.Helper()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	f.store.native.SeedSteps(r.ID, []stepmodel.RunStep{{Path: "build", Name: "build", Status: status, DisplayReason: reason, Detail: "Complete native diagnostics."}})
	got, err := f.s.RunGet(t.Context(), caller(), viewQuery{ID: r.ID})
	if err != nil || len(got.Steps) != 1 {
		t.Fatalf("step read: %+v %v", got, err)
	}
	if step := got.Steps[0]; step.Status != status || step.DisplayReason != want || step.Detail != "Complete native diagnostics." {
		t.Fatalf("step diagnostics: %+v, want %q", step, want)
	}
	raw, err := f.store.native.Steps(t.Context(), r.ID)
	if err != nil || len(raw) != 1 || raw[0].DisplayReason != reason {
		t.Fatalf("read overwrote native reason: %+v %v", raw, err)
	}
}
