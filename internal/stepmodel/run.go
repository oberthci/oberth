package stepmodel

import "time"

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
