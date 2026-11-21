package model

import (
	"time"

	"github.com/oberthci/oberth/internal/actor"
)

// RunCause is what started a run, as recorded at acceptance: the trigger, the
// identity that caused it, the schedule entry of a scheduled run and the run it
// reruns. Run views show it unchanged.
type RunCause struct {
	Actor         string     `json:"actor"`
	Trigger       RunTrigger `json:"trigger"`
	ScheduleEntry string     `json:"schedule_entry"`
	RerunOf       string     `json:"rerun_of"`
}

// RunOrigin holds attribution and execution identity alongside stepmodel.RunSummary.
// C7 persists assist opt-in and the monotonic Assisted flag separately from verdict.
type RunOrigin struct {
	RunCause
	AssistRequested bool   `json:"assist_requested"`
	Assisted        bool   `json:"assisted"`
	WorkflowName    string `json:"workflow_name"`
	WorkflowUID     string `json:"workflow_uid"`
}

// ReleaseReceipt identifies a delivered release, never a merely passing run.
type ReleaseReceipt struct {
	Tag           string `json:"tag"`
	CommitSHA     string `json:"commit_sha"`
	RunID         string `json:"run_id"`
	PublicationID string `json:"publication_id"`
}

// Publication binds an exact object to a predecessor; nil is unknown, empty is known absent.
type Publication struct {
	Detail      string          `json:"detail,omitempty"`
	Release     *ReleaseReceipt `json:"release,omitempty"`
	ID          string          `json:"id"`
	Repo        string          `json:"repo"`
	Ref         string          `json:"ref"`
	RunID       string          `json:"run_id"`
	PromotionID string          `json:"promotion_id"`
	ObjectSHA   string          `json:"object_sha"`
	CommitSHA   *string         `json:"commit_sha"`
	// PreviousSHA is nil when unknown; a pointer to empty means known absent.
	PreviousSHA *string           `json:"previous_sha"`
	Actor       string            `json:"actor"`
	Status      PublicationStatus `json:"status"`
	Attempts    int               `json:"attempts"`
	RetryAt     *time.Time        `json:"retry_at"`
	Reason      string            `json:"reason"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

func (v Publication) Clone() Publication {
	v.CommitSHA = copyPointer(v.CommitSHA)
	v.PreviousSHA = copyPointer(v.PreviousSHA)
	v.RetryAt = copyPointer(v.RetryAt)
	if v.Release != nil {
		receipt := *v.Release
		v.Release = &receipt
	}
	return v
}

// Promotion retains one source and the exact tested default-branch result.
// Caller is private durable attribution for bounded successor preparation.
type Promotion struct {
	ID            string          `json:"id"`
	Repo          string          `json:"repo"`
	SourceRunID   string          `json:"source_run_id"`
	SourceSHA     string          `json:"source_sha"`
	SourceRef     string          `json:"source_ref"`
	TargetRef     string          `json:"target_ref"`
	BaseSHA       string          `json:"base_sha"`
	ResultSHA     string          `json:"result_sha"`
	RunID         string          `json:"run_id"`
	PredecessorID string          `json:"predecessor_id"`
	Actor         string          `json:"actor"`
	Reason        string          `json:"reason"`
	Detail        string          `json:"detail,omitempty"`
	Revision      int64           `json:"revision"`
	Successors    int             `json:"successors"`
	Status        PromotionStatus `json:"status"`
	Caller        actor.Actor     `json:"-"`
}

func (p Promotion) Clone() Promotion { p.Caller = p.Caller.Clone(); return p }

// AuditAction contains only deliberately public targets and details, after the
// caller's redaction. Credential IDs and material do not belong in this value.
type AuditAction struct {
	ID           int64     `json:"id"`
	Actor        string    `json:"actor"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	Details      string    `json:"details"`
	CreatedAt    time.Time `json:"created_at"`
}

func copyPointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// HasAssistance excludes audited inspection dispatches from evidence.
// Requesting assist alone leaves a run ordinary.
func (o RunOrigin) HasAssistance() bool { return o.Assisted }
