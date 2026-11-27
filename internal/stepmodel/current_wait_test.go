package stepmodel

import "testing"

func TestRunningReasonSelectsCurrentWaitInPlanOrder(t *testing.T) {
	t.Parallel()
	steps := []RunStep{{Path: "executing", Status: StepRunning}, {Path: "waiting", Status: StepQueued, DisplayReason: "waiting for a node"}}
	status, reason := Progress(RunRunning, "starting containers", steps, "waiting to start")
	if status != RunRunning || reason != "waiting for a node" {
		t.Fatal("a running sibling hid the current step wait", status, reason)
	}
	steps[1].DisplayReason = "image pull failed"
	_, reason = Progress(status, reason, steps, "")
	if reason != "image pull failed" {
		t.Fatal("old waiting rank overrode a new native cause", reason)
	}
	steps[1].Status = StepPassed
	_, reason = Progress(status, reason, steps, "")
	if reason != "" {
		t.Fatal("completed wait revived a current run reason", reason)
	}
}
