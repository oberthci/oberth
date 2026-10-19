package model

// PhaseCancelled is the durable phase of a run an operator or agent
// cancelled. The run's status is RunInterrupted: cancellation reuses the
// existing terminal non-green status rather than adding one, so every client
// that already treats interrupted as terminal keeps doing so. Reason records
// who cancelled it and why.
const PhaseCancelled = "cancelled"

// MaximumCancelReasonBytes bounds the free-text reason an actor gives.
const MaximumCancelReasonBytes = 512

// Cancelled reports whether the run was terminalized by an explicit cancel.
func (run Run) Cancelled() bool {
	return run.Status == RunInterrupted && run.Phase == PhaseCancelled
}

// Preempted reports whether someone other than the run's owner terminalized
// it while it was active: a newer push or restart requeue superseded it, or an
// actor cancelled it. The owner of such a run settles its cancellation
// obligation and never records a result over it.
func (run Run) Preempted() bool {
	return run.Status == RunInterrupted && (run.SupersededBy != "" || run.Phase == PhaseCancelled)
}

// RunCancelRequest is one audited cancel of an active run.
type RunCancelRequest struct {
	RunID  string
	Actor  string
	Reason string
	// Force records that an admin explicitly overrode the release/apply
	// guard. The store records it in the audit details; the authorization
	// decision is the service layer's.
	Force bool
	// Warning, when set, is recorded in the audit details with the cancel.
	Warning string
}

// RunCancelResult is the terminalized run, the visible state it was cancelled
// from (queued, waiting or running), and the durable Workflow-deletion
// obligation recorded for a claimed run.
type RunCancelResult struct {
	Run           Run
	PreviousState RunStatus
	Cancellations []RunCancellation
	// PromotionID names the promotion the cancel failed, if the run was one.
	PromotionID string
}
