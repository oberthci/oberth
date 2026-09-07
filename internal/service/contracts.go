// Package service contains Oberth's control-plane orchestration. It depends on
// narrow contracts so Git and Kubernetes adapters can evolve independently.
package service

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
)

var (
	ErrAmbiguousRepository = errors.New("service: selector matches more than one repository")
	ErrForbidden           = errors.New("service: forbidden")
	ErrInvalidInput        = errors.New("service: invalid input")
	ErrUnavailable         = errors.New("service: capability unavailable")
	ErrReleaseUnreachable  = errors.New("service: tag commit is not reachable from the default branch")
)

const (
	defaultMaximumWait = 2 * time.Minute
	maximumToolBytes   = 1 << 20
	maximumIssuePage   = 50
)

type UplinkAuthenticator interface {
	Authenticate(context.Context, string) (model.AuthenticatedUplink, error)
}

type RepositoryLookup interface {
	RepositoryByName(context.Context, string) (model.Repository, error)
	ListRepositories(context.Context) ([]model.Repository, error)
}

type RepositoryWriter interface {
	RepositoryByName(context.Context, string) (model.Repository, error)
	CreateRepository(context.Context, model.RepositorySpec) (model.Repository, error)
	SetRepositoryDefaultBranch(context.Context, int64, string) (model.Repository, error)
}

type RepositoryReader interface {
	Repository(context.Context, int64) (model.Repository, error)
	RepositoryByName(context.Context, string) (model.Repository, error)
	ListRepositories(context.Context) ([]model.Repository, error)
}

// RepositoryRemover is the narrow contract for repository removal, requiring
// only the store's RemoveRepository and the git cache cleanup callback.
type RepositoryRemover interface {
	RemoveRepository(context.Context, string, string) (store.RemovedRepository, error)
}

type RunLookup interface {
	ResolveRun(context.Context, int64, string) (model.Run, error)
}

type RunHistory interface {
	ListRecentRuns(context.Context, model.RunListFilter) ([]model.Run, error)
	ListLatestRunsPerRepo(context.Context, int) ([]model.Run, error)
	Run(context.Context, string) (model.Run, error)
	StepResults(context.Context, string) ([]model.StepResult, error)
}

type RunQueueStore interface {
	EnqueueReceiveEvent(context.Context, model.ReceiveEventSpec, model.RunSpec) (model.EnqueueRunResult, error)
	ClaimNextRun(context.Context) (model.Run, error)
	Run(context.Context, string) (model.Run, error)
	SetRunJobName(context.Context, string, string) (model.Run, error)
	SetRunCredentialed(context.Context, string) error
	PendingRunCancellations(context.Context) ([]model.RunCancellation, error)
	CompleteRunCancellation(context.Context, string, string) error
	CompleteSupersededRunCancellationWithoutJob(context.Context, string) error
	FinishRun(context.Context, string, model.RunResult) (model.Run, error)
	PutStepResult(context.Context, model.StepResult) (model.StepResult, error)
	RunningRunsWithJobs(context.Context) ([]model.Run, error)
	// RequeueStrandedRun supersedes one stranded running run with a fresh
	// queued copy of the same spec (issue #270); store.ErrRequeueIneligible
	// signals the caller must terminalize instead.
	RequeueStrandedRun(context.Context, string) (model.Run, error)
}

type ReceiveRecorder interface {
	RecordReceiveEvent(context.Context, model.ReceiveEventSpec) (bool, error)
}

type PromotionRunStore interface {
	EnqueueAdmittedPromotionRun(context.Context, model.RunSpec, string) (model.EnqueueRunResult, model.Promotion, error)
	FinishRun(context.Context, string, model.RunResult) (model.Run, error)
}

type RunResolver interface {
	RepositoryLookup
	RunLookup
}

// BranchRefResolver is an optional capability a RunResolver may implement to
// enumerate distinct branch refs for a given (repository, SHA) pair. Sync
// uses it to reject bare-SHA requests that are ambiguous across branches.
type BranchRefResolver interface {
	DistinctBranchRefsForSHA(ctx context.Context, repoID int64, sha string) ([]string, error)
}

// RefResolver resolves a branch name to its current commit SHA from a local
// bare Git cache without contacting the upstream forge.
type RefResolver interface {
	RefSHA(ctx context.Context, repo string, branch string) (string, error)
}

type IssueRepository interface {
	CreateManualIssue(context.Context, string, model.ManualIssueSpec) (model.Issue, error)
	Issue(context.Context, int64) (model.Issue, error)
	UpdateIssue(context.Context, string, int64, model.IssuePatch) (model.Issue, error)
	UpdateManualIssue(context.Context, string, int64, model.IssuePatch) (model.Issue, error)
	DeleteManualIssue(context.Context, string, int64) error
	ListIssues(context.Context, model.IssueListFilter) (model.IssuePage, error)
	AcquireIssueLock(context.Context, int64, string) (model.IssueLock, error)
	RenewIssueLock(context.Context, int64, string) (model.IssueLock, error)
	OpenCIIssue(context.Context, int64, string) (model.Issue, error)
	CloseCIIssue(context.Context, string, int64, string) (model.Issue, error)
}

type PromotionRepository interface {
	PublicationStore
	WorkspaceCleanupStore
	AppendPromotion(context.Context, model.PromotionSpec) (model.Promotion, error)
	PlanPromotion(context.Context, string, string, string) (model.Promotion, error)
	Promotion(context.Context, string) (model.Promotion, error)
	PromotionByRun(context.Context, string) (model.Promotion, error)
	FinishPromotion(context.Context, string, model.PromotionStatus, string, string) (model.Promotion, error)
}

type PublicationStore interface {
	BeginPublication(context.Context, model.PublicationSpec) (model.Publication, error)
	SetPublicationPredecessor(context.Context, string, string, bool) (model.Publication, error)
	Publication(context.Context, string) (model.Publication, error)
	PendingPublications(context.Context) ([]model.Publication, error)
	FinalizePublication(context.Context, string, model.PublicationStatus, string) (model.PublicationFinalization, error)
}

// WorkspaceCleanupStore exposes only durable, read-only ownership decisions.
// Filesystem removal remains a service responsibility after these predicates
// prove that no run, cancellation, promotion, or publication can still use the
// corresponding workspace.
type WorkspaceCleanupStore interface {
	RunWorkspaceCleanupEligible(context.Context, string) (bool, error)
	PromotionWorkspaceCleanupEligible(context.Context, string) (bool, error)
	RunWorkspaceCleanupCandidates(context.Context, string, int) ([]string, error)
	PromotionWorkspaceCleanupCandidates(context.Context, string, int) ([]string, error)
}

type Auditor interface {
	AppendAuditAction(context.Context, model.AuditActionSpec) (model.AuditAction, error)
}

// SecretAccessStore is the narrow contract for the secret access approval
// table. The service layer uses it for MCP tool handlers; admission uses the
// store directly.
type SecretAccessStore interface {
	SecretAccessList(ctx context.Context, repo string, includeRevoked bool) ([]store.SecretAccessGrant, error)
	Grant(ctx context.Context, repo, step, secret, actor string) (store.SecretAccessGrant, error)
	Revoke(ctx context.Context, repo, step, secret, actor string) (store.SecretAccessGrant, error)
	// QualifiedRepoName resolves a repository ID to its canonical
	// "upstream/org/repo" form. Used by access grant/revoke handlers to
	// store grants with identity-safe keys (#245 BLOCKER B).
	QualifiedRepoName(ctx context.Context, repoID int64) (string, error)
	// RepositoryByName resolves any accepted repository spelling (bare,
	// org/repo, upstream/org/repo) to the registered repository. The
	// access reconciler uses it to canonicalize ConfigMap grant entries
	// at the parse boundary, so a bare-spelled entry converges onto the
	// same qualified persisted key the migrations and API handlers write
	// instead of endlessly re-creating bare rows (#245 BLOCKER B).
	RepositoryByName(ctx context.Context, name string) (model.Repository, error)
}

type SchedulerStore interface {
	Repository(context.Context, int64) (model.Repository, error)
	// Upstream resolves a repository's registered upstream so release
	// admission can bind the org identity for hierarchical secret path
	// scoping. Required, not optional: an unavailable lookup fails the
	// release closed before any secret fetch.
	Upstream(context.Context, int64) (model.Upstream, error)
	RunQueueStore
	IssueRepository
	PromotionRepository
	WorkspaceCleanupStore
}

type RepositoryDiscoverer interface {
	DiscoverRepository(context.Context, string) (model.RepositorySpec, error)
}

type PushGit interface {
	Ensure(context.Context, string) (gitcache.Repository, error)
	PeelObject(context.Context, string, string) (gitcache.PeeledObject, error)
	ReleaseReachable(context.Context, string, string, string) (bool, error)
	RemoteRef(context.Context, string, string) (string, bool, error)
	DeleteBranch(context.Context, string, string) error
}

type PublicationGit interface {
	RemoteRef(context.Context, string, string) (string, bool, error)
	SyncBranch(context.Context, string, string, string) error
	SyncTag(context.Context, string, string, string) error
	PushPromotion(context.Context, string, string, string) error
}

type DeliveryGit interface {
	PublicationGit
	PreparePromotion(context.Context, string, string, string, string) (gitcache.MergeCandidate, error)
}

type WorkspaceGit interface {
	PublicationGit
	Checkout(context.Context, string, string, string) error
}

type CIRequest struct {
	EventID    string
	Repository model.Repository
	Branch     string
	OldSHA     string
	SHA        string
	Actor      string
	Trigger    string
	TestedSHA  string
	BaseSHA    string
}

type ReleaseRequest struct {
	EventID    string
	Repository model.Repository
	Tag        string
	OldSHA     string
	ObjectSHA  string
	CommitSHA  string
	Actor      string
}

// CIQueue and ReleaseAdmission are deliberately distinct capabilities. An
// untrusted transport cannot turn a branch request into a release by setting a
// boolean; only the ancestry-gated tag path receives ReleaseAdmission.
type CIQueue interface {
	EnqueueCI(context.Context, CIRequest) (model.EnqueueRunResult, error)
}

type ReleaseAdmission interface {
	AdmitRelease(context.Context, ReleaseRequest) (model.EnqueueRunResult, error)
}

type EnqueueObserver interface {
	AcceptEnqueue(context.Context, model.EnqueueRunResult) error
	NotifyQueue()
	DeliverPublication(context.Context, string) (model.PublicationFinalization, error)
	NotifyPublicationRecovery(string)
}

type LogStore interface {
	Create(string) (*os.File, error)
	BuildIndex(string) (runlog.Index, error)
	ReadFiltered(string, string, string, runlog.Filter) ([]byte, runlog.Meta, error)
	ReadActiveFiltered(string, string, string, runlog.Filter) ([]byte, runlog.Meta, error)
	Tail(string, int64) ([]byte, error)
	// ReadFrom serves the dashboard's live view of a run in progress: bytes
	// from offset (negative means tail), the next poll offset, and the
	// current size of the append-only run log.
	ReadFrom(string, int64, int64) ([]byte, int64, int64, error)
	AppendStepProgress(string, runprogress.Event) error
	StepProgress(string) ([]runprogress.Event, error)
	// WriteStepPlan records the run's declared step list once, at submission,
	// and StepPlan reads it back. A run with no plan returns an error wrapping
	// os.ErrNotExist, which readers treat as "this run predates its plan" and
	// not as a failure.
	WriteStepPlan(string, []runprogress.PlannedStep) error
	StepPlan(string) ([]runprogress.PlannedStep, error)
}

type JobRequest struct {
	JobName    string
	Run        model.Run
	Repository model.Repository
	// UpstreamName is the registered name of the repository's upstream,
	// resolved by the scheduler for all credentialed runs. Used to
	// construct canonical per-repo identity keys.
	UpstreamName string
	// UpstreamOrg is the registered org of the repository's upstream
	// (model.Upstream.Org), resolved by the scheduler for release runs and
	// empty otherwise. Release admission matches repository-declared
	// oberth/upstream/ secret paths against it; the value derives from
	// administrator-owned registration state, never from the pushed source.
	UpstreamOrg string
	SourceDir   string
	Size        periapsis.Size
}

type JobResult struct {
	Status     model.RunStatus
	Phase      string
	TestedSHA  string
	BaseSHA    string
	FailedBurn string
	FailedStep string
	Error      string
	Steps      []model.StepResult
}

// ErrJobNotTerminal is returned by TerminalResult when the named Job has not
// reached a terminal Kubernetes state (either absent or still active).
var ErrJobNotTerminal = errors.New("service: job has not reached terminal state")

type JobController interface {
	CreateCI(context.Context, JobRequest) error
	Wait(context.Context, string, io.Writer) (JobResult, error)
	Delete(context.Context, string, string) error
	// TerminalResult checks whether a named K8s Job has reached a terminal
	// state. Returns (result, nil) when the Job completed or failed; returns
	// (_, ErrJobNotTerminal) when the Job is absent or still active; returns
	// (_, err) on transient API failure.
	TerminalResult(context.Context, string) (JobResult, error)
}

type ReleaseJobController interface {
	CreateRelease(context.Context, JobRequest) error
}

// PipelineSizer statically reads the immutable checked-out pipeline without
// evaluating repository code in the server process.
type PipelineSizer interface {
	PipelineSize(context.Context, JobRequest) (periapsis.Size, error)
}

// PipelinePlanner enumerates the steps the immutable checked-out pipeline
// declares, by the same static read PipelineSizer makes: the server reads the
// reviewed document, never evaluates it.
//
// It is optional exactly as PipelineSizer is. An engine that cannot enumerate
// its own format simply has no plan, and its runs behave as they always have:
// steps appear as execution reaches them.
type PipelinePlanner interface {
	PlannedSteps(context.Context, JobRequest) ([]runprogress.PlannedStep, error)
}

// PipelineCredentialDetector reports whether the checked-out pipeline declares
// secret-store paths. Like PipelineSizer, it reads the reviewed document
// without evaluating it. An engine that does not implement this interface is
// assumed to have no credentialed CI pipelines; release runs are always
// credentialed regardless.
type PipelineCredentialDetector interface {
	PipelineCredentialed(context.Context, JobRequest) (bool, error)
}

type Health interface {
	Ready(context.Context) error
	Status(context.Context) (any, error)
}

type BranchPush struct {
	EventID    string
	Repository string
	Branch     string
	OldOID     string
	NewOID     string
	Actor      string
}

type TagPush struct {
	EventID             string
	Repository          string
	Tag                 string
	OldOID              string
	NewOID              string
	Actor               string
	ReleaseAdmissionSHA string
}

type PushResult struct {
	Repository    model.Repository        `json:"repository"`
	Run           *model.Run              `json:"run,omitempty"`
	Cancellations []model.RunCancellation `json:"cancellations,omitempty"`
	Deleted       bool                    `json:"deleted"`
	Admitted      bool                    `json:"admitted"`
	Duplicate     bool                    `json:"duplicate"`
	Reason        string                  `json:"reason,omitempty"`
}

type StatusResponse struct {
	Repository model.Repository   `json:"-"`
	Run        model.Run          `json:"-"`
	Steps      []model.StepResult `json:"-"`
	Repo       string             `json:"repo"`
	Ref        string             `json:"ref"`
	SHA        string             `json:"sha"`
	RunID      string             `json:"run"`
	Status     string             `json:"status"`
	FailedStep string             `json:"failed_step,omitempty"`
	ExitCode   *int               `json:"exit_code,omitempty"`
	Issue      *int64             `json:"issue,omitempty"`
	Burns      map[string]string  `json:"burns"`
}

type LogResponse struct {
	RunID         string `json:"run_id"`
	Burn          string `json:"burn"`
	Step          string `json:"step,omitempty"`
	Output        string `json:"output"`
	TotalLines    int    `json:"total_lines"`
	MatchedLines  int    `json:"matched_lines"`
	ReturnedLines int    `json:"returned_lines"`
	Truncated     bool   `json:"truncated"`
	Bytes         int64  `json:"bytes"`
}

// LiveLogResponse is one polled slice of a running Job's redacted log stream.
// Offset is the value to pass on the next poll; Done reports whether the run
// reached a terminal state (read before the chunk, so a terminal answer can
// never precede unread bytes).
type LiveLogResponse struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
	Chunk  string `json:"chunk"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	Done   bool   `json:"done"`
}

// RunDetailResponse is the read-only dashboard view of one run: the durable
// run record, its recorded burn/step results, and the owning repository. It
// intentionally mirrors the raw /api/runs list encoding (Go field names) so
// the dashboard reads one consistent shape.
type RunDetailResponse struct {
	Repository model.Repository
	Run        model.Run
	Steps      []model.StepResult
}

// MCPToolText keeps the MCP text block as the exact bounded log slice while
// structuredContent retains the range metadata.
func (response LogResponse) MCPToolText() string { return response.Output }

type WaitResponse struct {
	StatusResponse
	StillRunning bool `json:"still_running"`
}

type PromotionWaitResponse struct {
	Promotion    model.Promotion `json:"promotion"`
	StillRunning bool            `json:"still_running"`
}

// RepoView is the enriched repository response for the /api/repos dashboard
// endpoint. It embeds the repository record and attaches the most recent runs
// so the frontend never has to derive per-repo status from a global window.
type RepoView struct {
	model.Repository
	LatestRuns []model.Run `json:"LatestRuns"`
}
