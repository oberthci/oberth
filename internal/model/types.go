// Package model contains Oberth's durable domain records. The types deliberately
// contain no database or control-plane behavior so every transport can share the
// same persisted contract.
package model

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/oberthci/oberth/pkg/pipeline"
)

type RunStatus string

const (
	RunQueued RunStatus = "queued"
	// RunWaiting is a read-side projection for a claimed run whose Workflow
	// has not been submitted. The durable store keeps its claimed state running.
	RunWaiting     RunStatus = "waiting"
	RunRunning     RunStatus = "running"
	RunPassed      RunStatus = "passed"
	RunFailed      RunStatus = "failed"
	RunInterrupted RunStatus = "interrupted"
)

// Valid accepts only durable store statuses. Waiting remains a legacy wire
// projection for claimed runs before submission; display_status presents them
// as queued. Waiting must never pass validation for a persisted run status.
func (s RunStatus) Valid() bool {
	switch s {
	case RunQueued, RunRunning, RunPassed, RunFailed, RunInterrupted:
		return true
	default:
		return false
	}
}

func (s RunStatus) Active() bool { return s == RunQueued || s == RunWaiting || s == RunRunning }

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
	return CanonicalRepoKey(upstream.Name, upstream.Org(), name)
}

// CanonicalRepoKey is shared by repository routes, grant declarations and
// identity selection. An upstream without an org uses its own name as the org;
// callers without any upstream identity retain the legacy bare spelling.
func CanonicalRepoKey(upstream, org, repo string) string {
	if upstream == "" {
		return repo
	}
	if org == "" {
		org = upstream
	}
	return upstream + "/" + org + "/" + repo
}

type UpstreamSpec struct {
	Name    string
	Kind    string
	BaseURL string
	KeyName string
}

type Repository struct {
	ArchivedAt    *time.Time `json:",omitempty"`
	ArchivedBy    string     `json:",omitempty"`
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
	SourceBindings        []pipeline.PinnedSource `json:"source_bindings,omitempty"`
	IssueTrailers         []CommitIssueTrailer    `json:"issue_trailers,omitempty"`
	IssueTrailersRecorded bool                    `json:"-"`
	// FailureCause is private server-classified presentation metadata.
	FailureCause string `json:"-"`
	// Read-side identity and admission context; never part of the durable run row.
	Repo       string
	Superseded bool `json:"superseded"`

	// ResourceMessage is advice, separate from the exact status vocabulary.
	ResourceMessage string `json:"resource_message,omitempty"`

	// AssistRequested is the run's opt-in to retry holds; Assisted is set once
	// a held step was continued, and Assist holds the hold record. An
	// assisted run never satisfies promotion, publication or green-for-SHA.
	AssistRequested bool             `json:"assist_requested,omitempty"`
	Assisted        bool             `json:"assisted,omitempty"`
	Assist          *RunAssistRecord `json:"assist,omitempty"`

	EarlierAttempts int `json:"earlier_attempts,omitempty"`

	// The explicit admin fallback survives a withheld automatic tag boost.

	// Only accepted explicit nonstandard requests provide a tag fallback.

	ID            string
	QueueSequence int64
	// AdmissionSequence retains restart FIFO order; QueueSequence is the cursor.
	AdmissionSequence int64 `json:"-"`
	// Display fields are read-side projections; durable Status is unchanged.
	DisplayStatus string         `json:"display_status,omitempty"`
	DisplayReason string         `json:"display_reason"`
	Scheduling    *RunScheduling `json:"Scheduling,omitempty"`
	RepoID        int64
	RefKind       RefKind
	Ref           string
	SHA           string
	Actor         string
	Release       bool
	Trigger       string
	ScheduleEntry string `json:",omitempty"`
	Phase         string
	WorkflowName  string `json:"JobName"`
	TestedSHA     string
	BaseSHA       string
	FailedStage   string `json:"FailedBurn"`
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

	// Assist asks for retry holds (push option oberth.assist=1, rerun assist).
	Assist        bool
	Reason        string
	RepoID        int64
	RefKind       RefKind
	Ref           string
	SHA           string
	Actor         string
	Release       bool
	Trigger       string
	ScheduleEntry string
	TestedSHA     string
	BaseSHA       string
}

type RunResult struct {
	// FailureCause is server-classified summary metadata in the dedicated failure_cause column.
	// The complete error stays in Error. It never changes terminal status.
	FailureCause string `json:"-"`

	TerminalObservation *RunTerminalObservation
	Status              RunStatus
	Phase               string
	TestedSHA           string
	BaseSHA             string
	FailedStage         string `json:"FailedBurn"`
	FailedStep          string
	Error               string
	// FailureTail is transient issue-projection input. Store implementations must
	// not persist it in the run record or expose it through run status.
	FailureTail string
}

type RunListFilter struct {
	Status       RunStatusFilter
	Superseded   bool
	LatestPerRef bool
	RepoID       int64
	Ref          string
	// RefGlob keeps runs whose ref matches this path.Match pattern (#1524).
	RefGlob string
	Limit   int
	// Before is an exclusive QueueSequence cursor: zero starts at the newest
	// run, a page's last QueueSequence fetches the next older page.
	Before int64
	// BeforeRun is the resolved run ID for the default list's time cursor.
	BeforeRun string
	// RepoIDs, when non-nil, keeps only runs of these repositories; an empty
	// slice matches none. Nil lists every repository (#073).
	RepoIDs []int64
	// Terminal keeps only finished runs (passed, failed, interrupted): the
	// history view, which never repeats an active run.
	Terminal bool
	// Actor, when set, keeps only runs whose actor is this uplink identity
	// (run_list own).
	Actor string
}

// RunCounts is the number of retained runs, in total and per repository ID.
type RunCounts struct {
	Total         int
	ByRepo        map[int64]int
	Queued        int
	Running       int
	QueuedByRepo  map[int64]int
	RunningByRepo map[int64]int
}

type RunCancellation struct {
	RunID        string
	WorkflowName string `json:"JobName"`
	SupersededBy string
	Reason       string
	CreatedAt    time.Time
	CompletedAt  *time.Time
}

type EnqueueRunResult struct {
	Run
	Cancellations   []RunCancellation
	SupersededCount int64
	Duplicate       bool
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
	// StepPending is a declaration-only state; native Argo Pending nodes map
	// to StepQueued. A declaration cannot be written as an observed node.
	StepPending StepStatus = "pending"
	StepQueued  StepStatus = "queued"
	StepRunning StepStatus = "running"
	StepPassed  StepStatus = "passed"
	StepFailed  StepStatus = "failed"
	// StepSkipped includes a terminal run's declared steps that never executed,
	// including those unreached after an engine error or cancellation. It is
	// broader than Argo's when-condition Skipped phase.
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

// StepCredentialEvidence is the admitted template's frozen credential scope.
// Grant names/paths are locators, never credential values.
type StepCredentialEvidence struct {
	Known        bool     `json:"known"`
	Complete     bool     `json:"complete"`
	Template     string   `json:"template"`
	Credentialed bool     `json:"credentialed"`
	Grants       []string `json:"grants"`
}

// StepResult is one observed Argo status node, keyed by RunID and NodeID.
// Display and template names never establish step identity.
type StepResult struct {
	ReplayEvidenceComplete bool
	// LocalTemplate binds the observed node to a template in this admitted
	// Workflow. Referenced/external templates cannot borrow a same-name
	// inline template's credential-free evidence.
	LocalTemplate           bool `json:"-"`
	CredentialEvidenceKnown bool
	Credentialed            bool
	GrantEvidence           []string
	NodeID                  string
	DisplayName             string
	Template                string
	NodeType                string
	BoundaryID              string
	Phase                   string
	Message                 string
	PodName                 string
	Termination             *StepDiagnostic
	RunID                   string
	Stage                   string `json:"Burn"`
	Step                    string
	Ordinal                 int
	Status                  StepStatus
	ExitCode                int
	StartedAt               *time.Time
	FinishedAt              *time.Time
	RecordedAt              time.Time
	// Secrets is a display projection from the retained grant evidence.
	Secrets bool
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
	// ObservedTargetSHA is the exact forge head read before publication settled.
	ObservedTargetSHA string `json:"observed_target_sha,omitempty"`
	BranchCISkipped   bool
	Train             *PromotionTrain `json:"train,omitempty"`
	ID                string
	Sequence          int64
	RepoID            int64
	SourceBranch      string
	SourceSHA         string
	TargetRef         string
	PreviousSHA       string
	ResultSHA         string
	Actor             string
	Status            PromotionStatus
	RunID             string
	GrantID           string
	Error             string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// PromotionListFilter uses exact scope filters and an exclusive sequence cursor.
type PromotionListFilter struct {
	RepoID    int64
	SourceSHA string
	Status    PromotionStatus
	Before    int64
	Limit     int
	// RepoIDs, when non-nil, keeps only promotions of these repositories;
	// an empty slice matches none (#073).
	RepoIDs []int64
}

type PromotionPage struct {
	Promotions []Promotion
	NextBefore int64
}

type PromotionSpec struct {
	BranchCISkipped bool
	Train           *PromotionTrain
	RepoID          int64
	SourceBranch    string
	SourceSHA       string
	TargetRef       string
	PreviousSHA     string
	ResultSHA       string
	Actor           string
	GrantID         string
	// RetryOf names the failed or interrupted promotion this one re-runs
	// (#1097): the same repository, source SHA and target, merged afresh onto
	// the target's current head and re-tested. Empty for a fresh promote.
	RetryOf string
	// RetryMode is PromotionRetryManual (promote_retry) or PromotionRetryAuto
	// (the server's own retry after the target moved). Required with RetryOf.
	RetryMode string
}

// Promotion retry modes recorded in the promotion.retry audit action.
const (
	PromotionRetryManual = "manual"
	PromotionRetryAuto   = "auto"
)

// PromotionLineage is a promotion's retry lineage, recorded in the audit
// chain (promotion.retry) because promotion rows are immutable. RetryOf is the
// promotion it re-runs, RetriedAs the promotion that re-ran it, and
// AutoAttempt counts consecutive automatic retries ending at this promotion.
type PromotionLineage struct {
	RetryOf     string
	Mode        string
	AutoAttempt int
	RetriedAs   string
}

// PromotionGrant is an append-only, audited record that authorizes a non-admin
// uplink to promote an exact (repository, SHA, target) tuple. Created by an
// admin uplink. A promotion admitted under it holds it while pending, and it
// is consumed atomically with that promotion's delivered publication (#1097):
// a promotion that fails or is interrupted before delivery releases it, so a
// retry of the same tuple can use it again, but at most one promotion can hold
// it at a time and it backs at most one delivery.
type PromotionGrant struct {
	ID                string
	RepoID            int64
	SourceSHA         string
	TargetRef         string
	Issuer            string
	IssuerFingerprint string
	Note              string
	CreatedAt         time.Time
	ExpiresAt         time.Time
	RevokedBy         string
	RevokedAt         *time.Time
	ConsumedBy        string // Promotion ID
	ConsumedAt        *time.Time
}

// Active reports whether the grant is neither expired, revoked, nor consumed.
func (g PromotionGrant) Active(now time.Time) bool {
	return g.RevokedAt == nil && g.ConsumedAt == nil && now.Before(g.ExpiresAt)
}

type PromotionGrantSpec struct {
	RepoID            int64
	SourceSHA         string
	TargetRef         string
	Issuer            string
	IssuerFingerprint string
	Note              string
	ExpiresAt         time.Time
}

type PromotionGrantListFilter struct {
	Status    string // active, revoked, expired, consumed, all; empty preserves Active
	RepoID    int64
	SourceSHA string
	Active    *bool // nil = all, true = active only, false = inactive only
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
	PublicationAbandoned PublicationStatus = "abandoned"
)

func (s PublicationStatus) Valid() bool {
	switch s {
	case PublicationPending, PublicationDelivered, PublicationFailed, PublicationAbandoned:
		return true
	default:
		return false
	}
}

func (s PublicationStatus) Terminal() bool { return s.Valid() && s != PublicationPending }

type Publication struct {
	Attempts      int
	RetryAfter    time.Time
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

	// ArtifactCollectionDeadline carries the fixed evidence note into the
	// durable publication transaction. It does not affect the Git mutation.
	ArtifactCollectionDeadline bool
}

// PublicationFinalization is returned from the one transaction that closes a
// publication obligation and its owning run and/or promotion. Empty owner
// records mean that kind of owner was not attached to the publication.
type PublicationFinalization struct {
	// CleanupWarning reports post-commit local cleanup deferred to a later refresh.
	CleanupWarning string `json:"-"`
	Publication    Publication
	Run            Run
	Promotion      Promotion
	// TargetMovedTo is set, only by the delivery that finalized it, when a
	// promotion publication failed because the remote target held this
	// head instead of the planned base (#1097). It is not persisted.
	TargetMovedTo string
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

// IssueWorkStatus coordinates work independently of the open/closed lifecycle.
type IssueWorkStatus string

const (
	IssueTodo    IssueWorkStatus = "todo"
	IssueActive  IssueWorkStatus = "active"
	IssueWaiting IssueWorkStatus = "waiting"
	IssueReview  IssueWorkStatus = "review"
)

func (s IssueWorkStatus) Valid() bool {
	return s == IssueTodo || s == IssueActive || s == IssueWaiting || s == IssueReview
}

type Issue struct {
	WorkStatus IssueWorkStatus `json:"work_status"`
	// Stale is a transient service observation; reading never updates the issue.
	Stale    bool  `json:"stale"`
	Revision int64 `json:"revision"`
	// Lock is transient API observation, populated after visibility checks.
	Lock           *IssueLock `json:"lock"`
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
	// ScopeUpstreamID is the upstream a manual issue is filed under, or
	// zero (#073). CI issues belong to RepoID's upstream.
	ScopeUpstreamID int64
	Upstream        string `json:"upstream"` // Resolved for issue views; empty for workspace-global issues.
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ClosedAt        *time.Time
}

type ManualIssueSpec struct {
	Title string
	Body  string
	// ScopeUpstreamID files the issue under one upstream, so uplinks scoped
	// to it see it (#073). Zero keeps the issue workspace-global.
	ScopeUpstreamID int64
}

type IssuePatch struct {
	WorkStatus         *IssueWorkStatus
	ExpectedBodySHA256 string
	ExpectedRevision   *int64
	Title              *string
	Body               *string
	State              *IssueState
	// ScopeUpstreamID moves a manual issue; zero means workspace-global.
	ScopeUpstreamID *int64
}

type IssueListFilter struct {
	WorkStatus IssueWorkStatus
	RepoID     int64
	Kind       IssueKind
	State      IssueState
	Query      string
	BeforeID   int64
	Limit      int
	// UpstreamIDs, when non-nil, keeps only issues of these upstreams: CI
	// issues by their repository, manual issues by the upstream they are
	// filed under. An empty slice matches none, and workspace-global manual
	// issues never match (#073).
	UpstreamIDs []int64
}

type IssuePage struct {
	Issues     []Issue
	NextBefore int64
	Total      int64       `json:"total"`
	Counts     IssueCounts `json:"counts"`
}

// IssueCounts are facets of the same repository, upstream and text scope.
// State counts retain the kind filter; kind counts retain the state filter.
type IssueCounts struct {
	Open   int64 `json:"open"`
	Closed int64 `json:"closed"`
	Manual int64 `json:"manual"`
	CI     int64 `json:"ci"`
}

type IssueLock struct {
	IssueID    int64     `json:"issue_id"`
	Owner      string    `json:"owner"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type Uplink struct {
	ID                int64
	Fingerprint       string
	Identity          string
	TokenCredentialID string
	AuthActor         string
	Admin             bool
	// Scope limits the uplink to the repositories of these upstreams, by
	// name. Empty means every repository (#073).
	Scope     []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type UplinkSpec struct {
	Fingerprint       string
	Identity          string
	TokenCredentialID string
	AuthActor         string
	Admin             bool
	Scope             []string
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
	// ExpiresAt is the absolute bearer expiry; nil preserves legacy lifetime.
	ExpiresAt *time.Time
}

type TokenCredentialSpec struct {
	ExpiresAt *time.Time
	Name      string
	Digest    []byte
}

type AuthenticatedUplink struct {
	Uplink
	TokenCredential TokenCredential
}
