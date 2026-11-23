package stepmodel

import (
	"slices"
	"time"
)

type LogSelector struct {
	NodeID    string `json:"node_id"`
	PodName   string `json:"pod_name"`
	Container string `json:"container"`
}

// RunStep is one logical step. DisplayReason is empty or a complete phrase of
// at most 40 characters, without ellipsis or free-form native text. Detail holds
// the full diagnostic message for on-demand display; views never truncate reasons.
type RunStep struct {
	Path             string        `json:"plan_path"`
	Name             string        `json:"name"`
	ParallelGroup    int           `json:"parallel_group,omitempty"`
	CredentialIntent bool          `json:"credential_intent"`
	Status           StepStatus    `json:"status"`
	DisplayReason    string        `json:"display_reason"`
	Detail           string        `json:"detail,omitempty"`
	StartedAt        *time.Time    `json:"started_at"`
	FinishedAt       *time.Time    `json:"finished_at"`
	AttemptCount     int           `json:"attempt_count"`
	Attempts         []StepAttempt `json:"attempts,omitzero"`
	Logs             []LogSelector `json:"logs,omitzero"`
}

// Started reports a step that ran or failed in any attempt: a step that is
// only pending or queued, waiting for a lock, a node or its containers, has
// not, nor has a step that was skipped or never reached.
func (s RunStep) Started() bool {
	if s.Status.started() {
		return true
	}
	for _, a := range s.Attempts {
		if a.Status.started() {
			return true
		}
	}
	return false
}

func (s StepStatus) started() bool { return s != StepPending && s != StepQueued && s != StepSkipped }

// Progress is the run-status rule for an unfinished run: it is queued until
// one of its steps has started and running from then on, never queued again,
// even while a retry waits. A queued run's reason is waiting, what it waits
// for; a running run's reason is empty.
func Progress(current RunStatus, steps []RunStep, waiting string) (RunStatus, string) {
	if current == RunRunning || slices.ContainsFunc(steps, RunStep.Started) {
		return RunRunning, ""
	}
	return RunQueued, waiting
}

// RunStart is a run's start: the earliest of its recorded start and the starts
// of its started steps, in UTC; a waiting step does not start the run. It
// returns recorded itself when no step started earlier.
func RunStart(recorded *time.Time, steps []RunStep) *time.Time {
	start := recorded
	for _, s := range steps {
		if s.Started() && s.StartedAt != nil && (start == nil || s.StartedAt.Before(*start)) {
			start = new(s.StartedAt.UTC())
		}
	}
	return start
}

// RunSummary is the public run projection. DisplayReason is empty or a complete
// phrase of at most 40 characters, without ellipsis or free-form native text.
// Detail holds full diagnostics for on-demand display; views never truncate reasons.
type RunSummary struct {
	ID            string       `json:"id"`
	Repo          string       `json:"repo"`
	Ref           string       `json:"ref"`
	ObjectSHA     string       `json:"object_sha"`
	CommitSHA     *string      `json:"commit_sha"`
	Status        RunStatus    `json:"status"` // queued|running|passed|failed|cancelled
	DisplayReason string       `json:"display_reason"`
	Detail        string       `json:"detail,omitempty"`
	NativePhase   *NativePhase `json:"native_phase,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	StartedAt     *time.Time   `json:"started_at"`
	FinishedAt    *time.Time   `json:"finished_at"`
	LogicalStepCounts
	Steps          []RunStep      `json:"steps,omitzero"`
	NextStepOffset *int           `json:"next_step_offset,omitempty"`
	Failure        *FailureRecord `json:"failure,omitempty"`
}
