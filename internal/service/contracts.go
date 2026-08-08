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
)

var (
	ErrAmbiguousRepository = errors.New("service: selector matches more than one repository")
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

type RunLookup interface {
	ResolveRun(context.Context, int64, string) (model.Run, error)
}

type RunHistory interface {
	ListRecentRuns(context.Context, model.RunListFilter) ([]model.Run, error)
	StepResults(context.Context, string) ([]model.StepResult, error)
}

type RunQueueStore interface {
	EnqueueRun(context.Context, model.RunSpec) (model.EnqueueRunResult, error)
	EnqueueReceiveEvent(context.Context, model.ReceiveEventSpec, model.RunSpec) (model.EnqueueRunResult, error)
	ClaimNextRun(context.Context) (model.Run, error)
	Run(context.Context, string) (model.Run, error)
	SetRunJobName(context.Context, string, string) (model.Run, error)
	PendingRunCancellations(context.Context) ([]model.RunCancellation, error)
	CompleteRunCancellation(context.Context, string, string) error
	CompleteSupersededRunCancellationWithoutJob(context.Context, string) error
	FinishRun(context.Context, string, model.RunResult) (model.Run, error)
	PutStepResult(context.Context, model.StepResult) (model.StepResult, error)
}

type ReceiveRecorder interface {
	RecordReceiveEvent(context.Context, model.ReceiveEventSpec) (bool, error)
}

type PromotionRunStore interface {
	EnqueuePromotionRun(context.Context, model.RunSpec, model.PromotionSpec) (model.EnqueueRunResult, model.Promotion, error)
	EnqueueAdmittedPromotionRun(context.Context, model.RunSpec, string) (model.EnqueueRunResult, model.Promotion, error)
	FinishRun(context.Context, string, model.RunResult) (model.Run, error)
}

type RunResolver interface {
	RepositoryLookup
	RunLookup
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
	ReleaseIssueLock(context.Context, int64, string) error
	OpenCIIssue(context.Context, int64, string) (model.Issue, error)
	UpsertCIIssue(context.Context, string, int64, string, string, string) (model.Issue, error)
	CloseCIIssue(context.Context, string, int64, string) (model.Issue, error)
}

type PromotionRepository interface {
	PublicationStore
	WorkspaceCleanupStore
	AppendPromotion(context.Context, model.PromotionSpec) (model.Promotion, error)
	AppendPromotionForRun(context.Context, model.PromotionSpec, string) (model.Promotion, error)
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

type SchedulerStore interface {
	Repository(context.Context, int64) (model.Repository, error)
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
	Read(string, string, string) ([]byte, error)
	Tail(string, int64) ([]byte, error)
}

type JobRequest struct {
	JobName    string
	Run        model.Run
	Repository model.Repository
	SourceDir  string
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

type JobController interface {
	CreateCI(context.Context, JobRequest) error
	Wait(context.Context, string, io.Writer) (JobResult, error)
	Delete(context.Context, string, string) error
}

type ReleaseJobController interface {
	CreateRelease(context.Context, JobRequest) error
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
	RunID  string `json:"run_id"`
	Burn   string `json:"burn"`
	Step   string `json:"step,omitempty"`
	Output string `json:"output"`
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
