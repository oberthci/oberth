package model

import "time"

// RunTerminalObservation is a complete terminal Workflow snapshot, never a
// synthesized scheduler result. It retains every step attempt, not just latest.
type RunTerminalObservation struct {
	Version      int               `json:"version"`
	WorkflowName string            `json:"job_name"`
	Nodes        int               `json:"nodes"`
	Steps        []RunObservedStep `json:"steps"`
}
type RunObservedStep struct {
	// Evicted is set only from owned kubelet/Argo eviction evidence.
	Evicted    bool       `json:"evicted,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Stage      string     `json:"burn"`
	Step       string     `json:"step"`
	Status     StepStatus `json:"status"`
	Started    bool       `json:"started"`
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
	PriorityRequest
	RunID, Actor string
	ReplayAudit  *RunReplayAudit
	// Authorize is required for tag/release retries and runs inside the transaction.
	Authorize func(Run, RunReplayEvidence) error
}

// RunRerunRequest asks the store to queue a new run at the same SHA and ref
// as a terminal, undelivered run (#1098). Authorization is the caller's; the
// store re-applies Authorize to the run as it is inside the rerun transaction.
type RunRerunRequest struct {
	PriorityRequest
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

// IsInfrastructureRunFailure is the terminal failure class accepted by run_retry.
// Cancellation/interruption and publication replay have separate policies.
func IsInfrastructureRunFailure(status RunStatus, phase, failedStep string) bool {
	return status == RunFailed && (phase == "infrastructure" || phase == "job") && failedStep == ""
}

// EvictionOnly requires a complete terminal snapshot whose every failed attempt
// has a kubelet eviction cause. Unknown, timed-out and earlier real failures
// cannot be hidden by a later eviction of the same logical step.
func (o *RunTerminalObservation) EvictionOnly() bool {
	if o == nil || o.Version != 1 || o.WorkflowName == "" || o.Nodes < len(o.Steps) || o.Nodes > 4096 {
		return false
	}
	found := false
	for _, step := range o.Steps {
		if !step.Status.Terminal() || step.Status == StepTimedOut {
			return false
		}
		if step.Status == StepFailed {
			if !step.Evicted || step.Stage == "" || step.Step == "" || step.StartedAt == nil || step.FinishedAt == nil {
				return false
			}
			found = true
		}
	}
	return found
}

// EvictedAttempt binds retained failure evidence to its exact observed attempt.
// It does not erase step status, logs, exit code or diagnostic history.
func (o *RunTerminalObservation) EvictedAttempt(stage, step string, started, finished *time.Time) bool {
	if o == nil || started == nil || finished == nil {
		return false
	}
	for _, attempt := range o.Steps {
		if attempt.Evicted && attempt.Status == StepFailed && attempt.Stage == stage && attempt.Step == step && attempt.StartedAt != nil && attempt.FinishedAt != nil && started.Equal(*attempt.StartedAt) && finished.Equal(*attempt.FinishedAt) {
			return true
		}
	}
	return false
}
