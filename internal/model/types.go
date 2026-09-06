// Package model contains Oberth's durable domain records. The types deliberately
// contain no database or control-plane behavior so every transport can share the
// same persisted contract.
package model

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/oberthci/oberth/pkg/periapsis"
)

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
	KeyName   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Org is the upstream's organization identity: the trailing path component of
// the registered base URL in its registered spelling (for example,
// "ssh://git@github.com/oberthci" -> "oberthci"; a local "/srv/git/acme" ->
// "acme"). It returns "" when the base URL is empty. This derivation
// backs org-qualified SSH path resolution and hierarchical secret path
// scoping. The installer's orgFromBaseURL (internal/installer/onboard.go)
// must agree for URL-type inputs; both extract the last path segment by
// different parsing strategies.
func (upstream Upstream) Org() string {
	base := strings.TrimSuffix(strings.TrimSpace(upstream.BaseURL), "/")
	if base == "" {
		return ""
	}
	if filepath.IsAbs(base) {
		return filepath.Base(base)
	}
	parts := strings.Split(base, "/")
	return parts[len(parts)-1]
}

// QualifiedRepo composes the canonical fully-qualified repository input
// ("<upstream>/<org>/<repo>") for a repository registered under this
// upstream. Internal consumers that iterate known repositories pass this
// form to the git cache and to name-based lookups so that same-named
// repositories under different upstreams stay unambiguous (issue #264).
// A missing org identity falls back to the upstream name, matching the
// schedule engine's historical dedup-key composition.
func (upstream Upstream) QualifiedRepo(name string) string {
	org := upstream.Org()
	if org == "" {
		org = upstream.Name
	}
	return upstream.Name + "/" + org + "/" + name
}

type UpstreamSpec struct {
	Name    string
	Kind    string
	BaseURL string
	KeyName string
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
	Credentialed  bool
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
	RepoID       int64
	RefKind      RefKind
	Ref          string
	SHA          string
	Actor        string
	Release      bool
	Credentialed bool
	Trigger      string
	TestedSHA    string
	BaseSHA      string
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
	Ref    string
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
	// StepPending is a step the reviewed pipeline declares that execution has
	// not reached. It is a projection state only: it is produced by merging a
	// run's durable plan over its results, and it is never a durable result --
	// PutStepResult refuses it, and the step_results CHECK constraint does not
	// admit it. A plan must never be able to record an outcome.
	StepPending  StepStatus = "pending"
	StepQueued   StepStatus = "queued"
	StepRunning  StepStatus = "running"
	StepPassed   StepStatus = "passed"
	StepFailed   StepStatus = "failed"
	StepSkipped  StepStatus = "skipped"
	StepTimedOut StepStatus = "timed_out"
)

func (s StepStatus) Valid() bool {
	switch s {
	case StepPending, StepQueued, StepRunning, StepPassed, StepFailed, StepSkipped, StepTimedOut:
		return true
	default:
		return false
	}
}

func (s StepStatus) Terminal() bool {
	return s.Valid() && s != StepPending && s != StepQueued && s != StepRunning
}

// Progress orders the step lifecycle so two observations of the same step can
// be reconciled without knowing which produced them: a seeded plan entry, a
// live progress marker, or a durable result. Higher is later; an unknown
// status is behind every real one, so it can never displace one.
//
// The order exists because a run's step list is assembled from three sources
// that each know a different amount. The plan knows the step exists. The
// progress journal knows it started. The durable result knows how it ended.
func (s StepStatus) Progress() int {
	switch s {
	case StepPending:
		return 0
	case StepQueued:
		return 1
	case StepRunning:
		return 2
	case StepPassed, StepFailed, StepSkipped, StepTimedOut:
		return 3
	default:
		return -1
	}
}

type StepResult struct {
	RunID        string
	Burn         string
	Step         string
	Ordinal      int
	Status       StepStatus
	ExitCode     int
	LogStart     int64
	LogEnd       int64
	DeclaredSize periapsis.Size
	MaxRSSBytes  int64
	UserCPU      time.Duration
	SystemCPU    time.Duration
	StartedAt    *time.Time
	FinishedAt   *time.Time
	RecordedAt   time.Time
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

// TrustedPlanStatus is the append-oriented lifecycle of one explicitly
// authorized plan artifact. Active states hold the durable backend lease.
type TrustedPlanStatus string

const (
	TrustedPlanAuthorized TrustedPlanStatus = "authorized"
	TrustedPlanReady      TrustedPlanStatus = "ready"
	TrustedPlanAttached   TrustedPlanStatus = "attached"
	TrustedPlanApplying   TrustedPlanStatus = "applying"
	TrustedPlanConsumed   TrustedPlanStatus = "consumed"
	TrustedPlanFailed     TrustedPlanStatus = "failed"
	TrustedPlanExpired    TrustedPlanStatus = "expired"
)

func (status TrustedPlanStatus) Valid() bool {
	switch status {
	case TrustedPlanAuthorized, TrustedPlanReady, TrustedPlanAttached, TrustedPlanApplying,
		TrustedPlanConsumed, TrustedPlanFailed, TrustedPlanExpired:
		return true
	default:
		return false
	}
}

func (status TrustedPlanStatus) Terminal() bool {
	return status == TrustedPlanConsumed || status == TrustedPlanFailed || status == TrustedPlanExpired
}

// TrustedPlan binds one green candidate and exact target predecessor to an
// encrypted artifact. Ciphertext is intentionally omitted from this model;
// only the artifact manager/store capability can read it.
type TrustedPlan struct {
	ID                string
	Sequence          int64
	RepoID            int64
	UpstreamID        int64
	SourceRef         string
	SourceSHA         string
	TargetRef         string
	BaseSHA           string
	ResultSHA         string
	GreenRunID        string
	PlanRunID         string
	PromotionID       string
	ApplyID           string
	ApplyEnqueueError string
	BackendIdentity   string
	BackendKey        string
	ToolDigest        string
	LockDigest        string
	ConfigDigest      string
	ArtifactDigest    string
	ArtifactSize      int64
	Actor             string
	Status            TrustedPlanStatus
	Error             string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	ConsumedAt        *time.Time
	UpdatedAt         time.Time
}

type TrustedPlanSpec struct {
	RepoID          int64
	UpstreamID      int64
	SourceRef       string
	SourceSHA       string
	TargetRef       string
	BaseSHA         string
	ResultSHA       string
	GreenRunID      string
	BackendIdentity string
	BackendKey      string
	ToolDigest      string
	LockDigest      string
	ConfigDigest    string
	Actor           string
	ExpiresAt       time.Time
}

type TrustedPlanArtifact struct {
	Ciphertext string
	Digest     string
	Size       int64
}

type TrustedApplyStatus string

const (
	TrustedApplyQueued      TrustedApplyStatus = "queued"
	TrustedApplyRunning     TrustedApplyStatus = "running"
	TrustedApplyPassed      TrustedApplyStatus = "passed"
	TrustedApplyFailed      TrustedApplyStatus = "failed"
	TrustedApplyInterrupted TrustedApplyStatus = "interrupted"
)

func (status TrustedApplyStatus) Valid() bool {
	switch status {
	case TrustedApplyQueued, TrustedApplyRunning, TrustedApplyPassed, TrustedApplyFailed, TrustedApplyInterrupted:
		return true
	default:
		return false
	}
}

func (status TrustedApplyStatus) Terminal() bool {
	return status.Valid() && status != TrustedApplyQueued && status != TrustedApplyRunning
}

// TrustedApply is status evidence distinct from the already-finalized Git
// promotion. A failed apply never rewrites promotion publication history.
type TrustedApply struct {
	ID             string
	Sequence       int64
	PlanID         string
	PromotionID    string
	RepoID         int64
	TargetRef      string
	SHA            string
	RunID          string
	ArtifactDigest string
	BackendKey     string
	Actor          string
	Status         TrustedApplyStatus
	Error          string
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	UpdatedAt      time.Time
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
	Admin             bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type UplinkSpec struct {
	Fingerprint       string
	Identity          string
	TokenCredentialID string
	AuthActor         string
	Admin             bool
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
