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
	RunQueued RunStatus = "queued"
	// RunWaiting is a read-side projection for a claimed run whose Workflow
	// has not been submitted. The durable store keeps its claimed state running.
	RunWaiting     RunStatus = "waiting"
	RunRunning     RunStatus = "running"
	RunPassed      RunStatus = "passed"
	RunFailed      RunStatus = "failed"
	RunInterrupted RunStatus = "interrupted"
)

// Valid accepts only durable store statuses. Waiting is a read-side projection
// and must never pass validation for a persisted run status.
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
	// ConcurrencyGroup is the repository-declared concurrency group the run
	// was admitted under (#658), recorded once the scheduler reads it from the
	// run's own checkout; empty when the pipeline declares none or the run has
	// not been read yet. The group is scoped to RepoID.
	ConcurrencyGroup string
	QueuedAt         time.Time
	StartedAt        *time.Time
	FinishedAt       *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
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
	// Before is an exclusive QueueSequence cursor: zero starts at the newest
	// run, a page's last QueueSequence fetches the next older page.
	Before int64
	// RepoIDs, when non-nil, keeps only runs of these repositories; an empty
	// slice matches none. Nil lists every repository (#073).
	RepoIDs []int64
	// Terminal keeps only finished runs (passed, failed, interrupted): the
	// history view, which never repeats an active run.
	Terminal bool
}

// RunCounts is the number of retained runs, in total and per repository ID.
type RunCounts struct {
	Total  int
	ByRepo map[int64]int
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
	Termination  *StepDiagnostic
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
	GrantID      string
	Error        string
	CreatedAt    time.Time
	UpdatedAt    time.Time
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
	RepoID       int64
	SourceBranch string
	SourceSHA    string
	TargetRef    string
	PreviousSHA  string
	ResultSHA    string
	Actor        string
	GrantID      string
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
	// TargetMovedTo is set, only by the delivery that finalized it, when a
	// promotion publication failed because the remote target held this
	// head instead of the planned base (#1097). It is not persisted.
	TargetMovedTo string
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
	// ScopeUpstreamID is the upstream a manual issue is filed under, or
	// zero (#073). CI issues belong to RepoID's upstream.
	ScopeUpstreamID int64
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
	// UpstreamIDs, when non-nil, keeps only issues of these upstreams: CI
	// issues by their repository, manual issues by the upstream they are
	// filed under. An empty slice matches none, and workspace-global manual
	// issues never match (#073).
	UpstreamIDs []int64
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
}

type TokenCredentialSpec struct {
	Name   string
	Digest []byte
}

type AuthenticatedUplink struct {
	Uplink
	TokenCredential TokenCredential
}
