package model

// RunRerunRequest asks the store to queue a new run at the same SHA and ref
// as a terminal, undelivered run (#1098). Authorization is the caller's; the
// store re-applies Authorize to the run as it is inside the rerun transaction.
type RunRerunRequest struct {
	RunID  string
	Actor  string
	Reason string
	// Force records that an admin explicitly confirmed a tag/release rerun.
	Force bool
	// Authorize, when set, is called inside the rerun transaction with the
	// run as it is now; its warning is recorded in the run.rerun audit
	// details, and an error aborts the rerun unchanged.
	Authorize func(Run) (string, error)
}

// RunRerunResult is the queued rerun and the warning recorded with it.
type RunRerunResult struct {
	Enqueued EnqueueRunResult
	Warning  string
}
