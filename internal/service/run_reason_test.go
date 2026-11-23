package service_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestRunReadsOnlyShowSpecificReasons(t *testing.T) {
	for _, status := range []stepmodel.RunStatus{stepmodel.RunQueued, stepmodel.RunRunning, stepmodel.RunPassed, stepmodel.RunFailed, stepmodel.RunCancelled} {
		for _, reason := range []string{"", string(status), "waiting for a node", "image pull failed", "cancelled by worker"} {
			t.Run(string(status)+"/"+reason, func(t *testing.T) {
				f := setup(t)
				r := f.seed(status)
				r.DisplayReason = reason
				f.store.native.SeedRun(r)
				want := reason
				if reason == string(status) {
					want = ""
				}
				requireRunReads(t, f, runRead{status, want, false})
			})
		}
	}
}

func TestRunReadsDropStatusSentencesAndKeepQueuedWait(t *testing.T) {
	for _, tc := range []struct {
		status       stepmodel.RunStatus
		reason, want string
	}{{stepmodel.RunRunning, "run is running", ""}, {stepmodel.RunPassed, "run passed", ""}, {stepmodel.RunFailed, "run failed", ""}, {stepmodel.RunCancelled, "run cancelled", ""}, {stepmodel.RunQueued, "waiting for execution", "waiting for execution"}} {
		t.Run(string(tc.status), func(t *testing.T) {
			f := setup(t)
			r := f.seed(tc.status)
			r.DisplayReason = tc.reason
			f.store.native.SeedRun(r)
			requireRunReads(t, f, runRead{tc.status, tc.want, false})
		})
	}
}
