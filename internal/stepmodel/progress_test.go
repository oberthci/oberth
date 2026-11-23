package stepmodel

import "testing"

func TestQueuedReasonNeverMovesBack(t *testing.T) {
	for _, c := range []struct{ first, next, want string }{
		{"starting containers", "waiting to start", "starting containers"},
		{"image pull failed", "waiting to start", "image pull failed"},
		{"waiting to start", "waiting for a run slot", "waiting for a run slot"},
		{"waiting to start", "waiting for sync limit go", "waiting for sync limit go"},
		{"waiting for a node", "waiting for mutex publish", "waiting for mutex publish"},
		{"starting containers", "", "starting containers"},
		{"waiting for a run slot", "waiting for sync limit go", "waiting for sync limit go"},
		{"waiting for mutex publish", "waiting to start", "waiting to start"},
		{"waiting to start", "starting containers", "starting containers"},
		{"starting containers", "image pull failed", "image pull failed"},
		{"image pull failed", "starting containers", "starting containers"},
	} {
		t.Run(c.first+"/"+c.next, func(t *testing.T) {
			steps := []RunStep{{Status: StepPending}}
			status, reason := Progress(RunQueued, "", steps, c.first)
			status, reason = Progress(status, reason, steps, c.next)
			if status != RunQueued || reason != c.want {
				t.Fatalf("got %s / %q, want queued / %q", status, reason, c.want)
			}
			steps[0].Status = StepRunning
			status, reason = Progress(status, reason, steps, c.next)
			if status != RunRunning || reason != "" {
				t.Fatalf("started step kept its wait: %s / %q", status, reason)
			}
			steps[0].Status = StepPending
			status, reason = Progress(status, reason, steps, c.first)
			if status != RunRunning || reason != "" {
				t.Fatalf("running run regressed: %s / %q", status, reason)
			}
		})
	}
}
