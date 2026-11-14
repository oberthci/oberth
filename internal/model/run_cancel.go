package model

// PhaseCancelled records an explicit cancellation. A submitted run stays running
// until Argo's terminal phase establishes its final status.
const PhaseCancelled = "cancelled"

const MaximumCancelReasonBytes = 512

// Cancelled reports durable Stop intent. Active runs retain this phase while
// Argo finishes; free-text reasons never authorize a cancellation.
func (run Run) Cancelled() bool { return run.Phase == PhaseCancelled }

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
	// Authorize, when set, is called inside the cancel transaction with the
	// run as it is now, so the decision cannot act on a stale read. Its
	// warning replaces Warning; an error aborts the cancel unchanged.
	Authorize func(Run) (string, error)
}

// RunCancelResult is the run carrying Stop intent, the visible state it was cancelled
// from (queued, waiting or running), and the durable Workflow-Stop
// obligation recorded for a claimed run.
type RunCancelResult struct {
	Run           Run
	PreviousState RunStatus
	Cancellations []RunCancellation
	// PromotionID names the promotion the cancel failed, if the run was one.
	PromotionID string
}
