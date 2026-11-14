package model

// The user-facing run vocabulary. Every durable status, phase and projection
// maps to exactly one of these five labels; anything finer (what a queued
// run waits for, why a run failed) is the reason line,
// never a second status. Raw Status and Phase stay on the wire for machines.
//
// A status says what to do with the run: wait (queued), watch (running), read
// the result (passed, failed), or read why it was cancelled.
const (
	DisplayQueued  = "queued"
	DisplayRunning = "running"
	DisplayPassed  = "passed"
	DisplayFailed  = "failed"

	DisplayCancelled = "cancelled"
)

// DisplayStatuses lists the whole user-facing vocabulary, in list order.
var DisplayStatuses = []string{DisplayQueued, DisplayRunning, DisplayPassed, DisplayFailed, DisplayCancelled}

// ClaimedUnstarted reports accepted preparation before Workflow submission.
// Preparation displays running. Only native Argo Pending projects queued.
func (run Run) ClaimedUnstarted() bool {
	return run.Status == RunRunning && run.StartedAt == nil && run.Phase != "publishing"
}

// DisplayRunStatus maps durable history to its base display label.
// Live execution observations project native Argo Pending as queued.
func DisplayRunStatus(run Run) string {
	switch run.Status {
	case RunQueued, RunWaiting, RunRunning:
		return DisplayRunning
	case RunPassed:
		return DisplayPassed
	case RunFailed:
		return DisplayFailed
	case RunInterrupted:
		return DisplayCancelled
	default:
		return string(run.Status)
	}
}
