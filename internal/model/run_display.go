package model

// The user-facing run vocabulary. Every durable status, phase and projection
// maps to exactly one of these seven labels; anything finer (what a queued
// run waits for, why a run failed, which run replaced it) is the reason line,
// never a second status. Raw Status and Phase stay on the wire for machines.
//
// A status says what to do with the run: wait (queued), watch (running), read
// the result (passed, failed), or follow the successor (superseded,
// infra-retried). Cancelled covers every interruption without a successor.
const (
	DisplayQueued       = "queued"
	DisplayRunning      = "running"
	DisplayPassed       = "passed"
	DisplayFailed       = "failed"
	DisplayInfraRetried = "infra-retried"
	DisplaySuperseded   = "superseded"
	DisplayCancelled    = "cancelled"
)

// DisplayStatuses lists the whole user-facing vocabulary, in list order.
var DisplayStatuses = []string{DisplayQueued, DisplayRunning, DisplayPassed, DisplayFailed, DisplayInfraRetried, DisplaySuperseded, DisplayCancelled}

// ClaimedUnstarted reports a run the scheduler has claimed but not admitted
// or submitted: durable status running with no started_at (phase waiting,
// preparing or a trusted preflight). The wire projects it as RunWaiting; the
// run list and the display status keep it in the queue, because it has not
// started. Publication keeps its start time, so it is never unstarted.
func (run Run) ClaimedUnstarted() bool {
	return run.Status == RunRunning && run.StartedAt == nil && run.Phase != "publishing"
}

// DisplayRunStatus is the single raw-to-display mapping. Every server surface
// (HTTP, MCP, CLI, dashboard) derives display_status here and nowhere else.
func DisplayRunStatus(run Run) string {
	switch run.Status {
	case RunQueued, RunWaiting:
		return DisplayQueued
	case RunRunning:
		if run.ClaimedUnstarted() {
			return DisplayQueued
		}
		return DisplayRunning
	case RunPassed:
		return DisplayPassed
	case RunFailed:
		if run.AutoRetriedBy != "" {
			return DisplayInfraRetried
		}
		return DisplayFailed
	case RunInterrupted:
		if run.Phase == PhaseCancelled || run.SupersededBy == "" {
			return DisplayCancelled
		}
		return DisplaySuperseded
	default:
		return string(run.Status)
	}
}
