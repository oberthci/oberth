package model

import "testing"

func TestExecutionDeadlineReasonRecognizesWatchdogCleanupPath(t *testing.T) {
	const watchdog = ExecutionDeadlineMessage + " Server watchdog expired: context deadline exceeded"
	for _, reason := range []string{watchdog, watchdog + "\ncleanup unavailable", "observe adopted Workflow oberth-run: " + watchdog} {
		if !IsExecutionDeadlineReason(reason) {
			t.Errorf("missed watchdog deadline: %q", reason)
		}
	}
	for _, reason := range []string{"context deadline exceeded", "refresh upstream trusted test policy: context deadline exceeded", ExecutionDeadlineMessage + " unrelated text", watchdog + " while refreshing policy"} {
		if IsExecutionDeadlineReason(reason) {
			t.Errorf("unrelated timeout classified as execution deadline: %q", reason)
		}
	}
}
