package model

// RunTerminalObservation is a complete terminal Workflow snapshot, never a
// synthesized scheduler result. It retains every step attempt, not just latest.
type RunTerminalObservation struct {
	Version int               `json:"version"`
	JobName string            `json:"job_name"`
	Nodes   int               `json:"nodes"`
	Steps   []RunObservedStep `json:"steps"`
}
type RunObservedStep struct {
	Burn    string     `json:"burn"`
	Step    string     `json:"step"`
	Status  StepStatus `json:"status"`
	Started bool       `json:"started"`
}
type RunReplayAttempt struct {
	Run                Run
	Steps              []StepResult
	PublicationStarted bool
	Observation        *RunTerminalObservation
}
type RunReplayAudit struct {
	TagMoved           *bool  `json:"tag_moved,omitempty"`
	Basis              string `json:"replay_basis"`
	RunsChecked        int    `json:"replay_runs_checked"`
	StepsChecked       int    `json:"replay_steps_checked"`
	PublicationStarted bool   `json:"publication_started"`
}

// RunReplayEvidence is read in the writer transaction that queues a replay.
type RunReplayEvidence struct {
	Chain              []RunReplayAttempt
	ChainComplete      bool
	Newest             bool
	Observation        *RunTerminalObservation
	Steps              []StepResult
	PublicationStarted bool
}

// RunRetryRequest queues an eligible newest infrastructure/interrupted attempt.
type RunRetryRequest struct {
	RunID, Actor string
	ReplayAudit  *RunReplayAudit
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
	// ReplayAudit is filled only by Authorize inside the writer transaction.
	ReplayAudit *RunReplayAudit
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
