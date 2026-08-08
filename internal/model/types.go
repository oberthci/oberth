// Package model contains Oberth's durable domain records. The types deliberately
// contain no database or control-plane behavior so every transport can share the
// same persisted contract.
package model

import "time"

type RunStatus string

const (
	RunQueued      RunStatus = "queued"
	RunRunning     RunStatus = "running"
	RunPassed      RunStatus = "passed"
	RunFailed      RunStatus = "failed"
	RunInterrupted RunStatus = "interrupted"
)

func (s RunStatus) Valid() bool {
	switch s {
	case RunQueued, RunRunning, RunPassed, RunFailed, RunInterrupted:
		return true
	default:
		return false
	}
}

func (s RunStatus) Active() bool { return s == RunQueued || s == RunRunning }

func (s RunStatus) Terminal() bool { return s.Valid() && !s.Active() }

type RefKind string

const (
	RefBranch RefKind = "branch"
	RefTag    RefKind = "tag"
)

func (k RefKind) Valid() bool { return k == RefBranch || k == RefTag }

type Upstream struct {
	ID        int64
	Name      string
	Kind      string
	BaseURL   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type UpstreamSpec struct {
	Name    string
	Kind    string
	BaseURL string
}

type Repository struct {
	ID            int64
	Name          string
	UpstreamID    int64
	DefaultBranch string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type RepositorySpec struct {
	Name          string
	UpstreamID    int64
	DefaultBranch string
}

type Run struct {
	ID            string
	QueueSequence int64
	RepoID        int64
	RefKind       RefKind
	Ref           string
	SHA           string
	Actor         string
	Release       bool
	Trigger       string
	Phase         string
	JobName       string
	TestedSHA     string
	BaseSHA       string
	FailedBurn    string
	FailedStep    string
	Error         string
	Status        RunStatus
	Reason        string
	SupersededBy  string
	QueuedAt      time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type RunSpec struct {
	RepoID    int64
	RefKind   RefKind
	Ref       string
	SHA       string
	Actor     string
	Release   bool
	Trigger   string
	TestedSHA string
	BaseSHA   string
}

type RunResult struct {
	Status     RunStatus
	Phase      string
	TestedSHA  string
	BaseSHA    string
	FailedBurn string
	FailedStep string
	Error      string
	// FailureTail is transient issue-projection input. Store implementations must
	// not persist it in the run record or expose it through run status.
	FailureTail string
}

type RunListFilter struct {
	RepoID int64
	Limit  int
}

type RunCancellation struct {
	RunID        string
	JobName      string
	SupersededBy string
	Reason       string
	CreatedAt    time.Time
	CompletedAt  *time.Time
}

type EnqueueRunResult struct {
	Run
	Cancellations []RunCancellation
	Duplicate     bool
}

type ReceiveEvent struct {
	ID        string
	Actor     string
	RepoID    int64
	RefKind   RefKind
	Ref       string
	OldSHA    string
	ObjectSHA string
	CommitSHA string
	Outcome   string
	RunID     string
	CreatedAt time.Time
}

type ReceiveEventSpec struct {
	ID        string
	Actor     string
	RepoID    int64
	RefKind   RefKind
	Ref       string
	OldSHA    string
	ObjectSHA string
	CommitSHA string
	Outcome   string
}

type StepStatus string

const (
	StepPassed   StepStatus = "passed"
	StepFailed   StepStatus = "failed"
	StepSkipped  StepStatus = "skipped"
	StepTimedOut StepStatus = "timed_out"
)

func (s StepStatus) Valid() bool {
	switch s {
	case StepPassed, StepFailed, StepSkipped, StepTimedOut:
		return true
	default:
		return false
	}
}

func (s StepStatus) Terminal() bool { return s.Valid() }

type StepResult struct {
	RunID      string
	Burn       string
	Step       string
	Ordinal    int
	Status     StepStatus
	ExitCode   int
	LogStart   int64
	LogEnd     int64
	StartedAt  *time.Time
	FinishedAt *time.Time
	RecordedAt time.Time
}

type PromotionStatus string

const (
	PromotionPending     PromotionStatus = "pending"
	PromotionPassed      PromotionStatus = "passed"
	PromotionFailed      PromotionStatus = "failed"
	PromotionInterrupted PromotionStatus = "interrupted"
)

func (s PromotionStatus) Valid() bool {
	switch s {
	case PromotionPending, PromotionPassed, PromotionFailed, PromotionInterrupted:
		return true
	default:
		return false
	}
}

func (s PromotionStatus) Terminal() bool { return s.Valid() && s != PromotionPending }

type Promotion struct {
	ID           string
	Sequence     int64
	RepoID       int64
	SourceBranch string
	SourceSHA    string
	TargetRef    string
	PreviousSHA  string
	ResultSHA    string
	Actor        string
	Status       PromotionStatus
	RunID        string
	Error        string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type PromotionSpec struct {
	RepoID       int64
	SourceBranch string
	SourceSHA    string
	TargetRef    string
	PreviousSHA  string
	ResultSHA    string
	Actor        string
}

// Publication is the durable handoff between a passing CI result and the
// externally visible Git ref. Pending records are recovery obligations. The
// intent and result are durable before publication-side network I/O; the exact
// predecessor is either known at admission or bound by the first observed ref
// before mutation, making one-shot crash recovery safe.
type PublicationStatus string

const (
	PublicationPending   PublicationStatus = "pending"
	PublicationDelivered PublicationStatus = "delivered"
	PublicationFailed    PublicationStatus = "failed"
)

func (s PublicationStatus) Valid() bool {
	switch s {
	case PublicationPending, PublicationDelivered, PublicationFailed:
		return true
	default:
		return false
	}
}

func (s PublicationStatus) Terminal() bool { return s.Valid() && s != PublicationPending }

type Publication struct {
	ID            string
	Sequence      int64
	RepoID        int64
	RunID         string
	PromotionID   string
	RefKind       RefKind
	Ref           string
	PreviousSHA   string
	PreviousKnown bool
	ResultSHA     string
	Actor         string
	Status        PublicationStatus
	Error         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type PublicationSpec struct {
	RepoID        int64
	RunID         string
	PromotionID   string
	RefKind       RefKind
	Ref           string
	PreviousSHA   string
	PreviousKnown bool
	ResultSHA     string
	Actor         string
}

// PublicationFinalization is returned from the one transaction that closes a
// publication obligation and its owning run and/or promotion. Empty owner
// records mean that kind of owner was not attached to the publication.
type PublicationFinalization struct {
	Publication Publication
	Run         Run
	Promotion   Promotion
}

type IssueKind string

const (
	IssueManual IssueKind = "manual"
	IssueCI     IssueKind = "ci"
)

func (k IssueKind) Valid() bool { return k == IssueManual || k == IssueCI }

type IssueState string

const (
	IssueOpen   IssueState = "open"
	IssueClosed IssueState = "closed"
)

func (s IssueState) Valid() bool { return s == IssueOpen || s == IssueClosed }

type Issue struct {
	ID             int64
	RepoID         int64 // Zero identifies a workspace-global issue.
	Kind           IssueKind
	Branch         string
	Title          string
	Body           string
	State          IssueState
	Occurrences    int64
	CIOrigin       string
	CIWorkSequence int64
	CIWorkID       string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ClosedAt       *time.Time
}

type ManualIssueSpec struct {
	Title string
	Body  string
}

type IssuePatch struct {
	Title *string
	Body  *string
	State *IssueState
}

type IssueListFilter struct {
	RepoID   int64
	Kind     IssueKind
	State    IssueState
	BeforeID int64
	Limit    int
}

type IssuePage struct {
	Issues     []Issue
	NextBefore int64
}

type IssueLock struct {
	IssueID    int64
	Owner      string
	AcquiredAt time.Time
	ExpiresAt  time.Time
}

type Uplink struct {
	ID                int64
	Fingerprint       string
	Identity          string
	TokenCredentialID string
	AuthActor         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type UplinkSpec struct {
	Fingerprint       string
	Identity          string
	TokenCredentialID string
	AuthActor         string
}

type AuditAction struct {
	ID             int64
	Actor          string
	Action         string
	ResourceType   string
	ResourceID     string
	Details        string
	PreviousSHA256 []byte
	SHA256         []byte
	CreatedAt      time.Time
}

type AuditActionSpec struct {
	Actor        string
	Action       string
	ResourceType string
	ResourceID   string
	Details      string
}

type AuditHead struct {
	ID     int64
	SHA256 []byte
}

type AuditAnchor struct {
	ID          int64
	AuditID     int64
	AuditSHA256 []byte
	TSAURL      string
	Receipt     []byte
	AnchoredAt  time.Time
	CreatedAt   time.Time
}

type AuditAnchorSpec struct {
	AuditID     int64
	AuditSHA256 []byte
	TSAURL      string
	Receipt     []byte
	AnchoredAt  time.Time
}

type AuditWitness struct {
	UUID         string
	LogIndex     int64
	IntegratedAt time.Time
	AuditID      int64
	AuditSHA256  []byte
	PreviousUUID string
}

type AuditWitnessIntent struct {
	Sequence     int
	AuditID      int64
	AuditSHA256  []byte
	PreviousUUID string
}

type TokenCredential struct {
	ID          string
	Name        string
	Digest      []byte
	CreatedAt   time.Time
	ActivatedAt *time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
}

type TokenCredentialSpec struct {
	Name   string
	Digest []byte
}

type AuthenticatedUplink struct {
	Uplink
	TokenCredential TokenCredential
}
