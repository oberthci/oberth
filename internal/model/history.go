package model

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/oberthci/oberth/internal/actor"
)

// RunCause is what started a run, as recorded at acceptance: the trigger, the
// identity that caused it, the schedule entry of a scheduled run, the run it
// reruns and whether the push asked for assistance. Run views show it unchanged.
type RunCause struct {
	Actor           string     `json:"actor"`
	Trigger         RunTrigger `json:"trigger"`
	ScheduleEntry   string     `json:"schedule_entry"`
	AssistRequested bool       `json:"assist_requested"`
	RerunOf         string     `json:"rerun_of"`
}

// RunOrigin holds attribution and execution identity alongside stepmodel.RunSummary.
// C7 persists assist opt-in and the monotonic Assisted flag separately from verdict.
type RunOrigin struct {
	RunCause
	// RequestedFile retains an explicit selector even when admission was refused.
	RequestedFile string `json:"requested_file,omitempty"`
	Assisted      bool   `json:"assisted"`
	WorkflowName  string `json:"workflow_name"`
	WorkflowUID   string `json:"workflow_uid"`
	// ResumedFrom names the failed run whose Workflow this run resumed on a
	// fixed commit. It is write-once; a resumed run is never evidence.
	ResumedFrom string          `json:"resumed_from,omitempty"`
	ResumeUID   string          `json:"resume_uid,omitempty"`
	ResumePlan  json.RawMessage `json:"resume_plan,omitempty"`
}

// ReleaseReceipt identifies a delivered release, never a merely passing run.
type ReleaseReceipt struct {
	Tag           string `json:"tag"`
	CommitSHA     string `json:"commit_sha"`
	RunID         string `json:"run_id"`
	PublicationID string `json:"publication_id"`
}

// MaxReceivedHeads bounds private receive-chain evidence.
const MaxReceivedHeads = 16

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
	// CIAfter is the newest run sequence when the first push attempt started,
	// before the ref can move: a delivered fast-forward's CI result is ordered
	// right after it. The stores keep it; no view shows it.
	// Zero is the valid bound for empty history.
	CIAfter int64 `json:"-"`
	// Attempted records that a push attempt started, even with empty history or
	// after an explicit retry reset the budget. It is durable and private.
	Attempted bool `json:"-"`
	// ReadBack marks a re-entered delivery, including an explicit retry. It does
	// not advance the durable attempt count and is never stored or sent.
	ReadBack bool `json:"-"`
	// ReceivedHeads is the capped set of heads replaced by this receive chain.
	// Nil is unknown; an empty string in the set records an absent ref.
	// It is durable private evidence, independent of the live push lease.
	ReceivedHeads []string `json:"-"`
}

func (v Publication) Clone() Publication {
	v.CommitSHA = copyPointer(v.CommitSHA)
	v.PreviousSHA = copyPointer(v.PreviousSHA)
	v.ReceivedHeads = slices.Clone(v.ReceivedHeads)
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

// HasAssistance excludes audited inspection dispatches and resumed runs from
// evidence. Requesting assist alone leaves a run ordinary.
func (o RunOrigin) HasAssistance() bool { return o.Assisted || o.Resumed() }

// Resumed reports a run that resumed a failed run's Workflow. It is no build
// of its commit in either direction: it neither gives nor blocks evidence.
func (o RunOrigin) Resumed() bool { return o.ResumedFrom != "" }

// NativeUID returns the Workflow identity of an ordinary run or a resume owner.
func (o RunOrigin) NativeUID() string {
	if o.Resumed() {
		return o.ResumeUID
	}
	return o.WorkflowUID
}
