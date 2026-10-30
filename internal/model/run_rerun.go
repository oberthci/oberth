package model

// RunReplayEvidence is read in the same writer transaction that queues a replay.
// Step labels (including Secrets) come from the immutable per-run plan instead.
type RunReplayEvidence struct {
	Steps              []StepResult
	PublicationStarted bool
}

// RunRetryRequest queues an eligible newest infrastructure/interrupted attempt.
type RunRetryRequest struct {
	RunID, Actor string
	// Authorize is required for tag/release retries and runs inside the transaction.
	Authorize func(Run, RunReplayEvidence) error
}

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
	// run and DB replay evidence as they are now. Its warning is recorded in
	// the run.rerun audit details; an error aborts the rerun unchanged.
	Authorize func(Run, RunReplayEvidence) (string, error)
}

// RunRerunResult is the queued rerun and the warning recorded with it.
type RunRerunResult struct {
	Enqueued EnqueueRunResult
	Warning  string
}
