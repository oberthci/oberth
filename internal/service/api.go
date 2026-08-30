package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
)

const promotionCompensationTimeout = 30 * time.Second

// revokePolicySyncAdvisory is the warning returned by access_revoke to inform
// the caller that the Vault credentialed policy still has the exact-path grant.
// The server's own identity has no Vault policy-write capability, so the resync
// must be triggered externally.
const revokePolicySyncAdvisory = "Revocation is effective for new Oberth admissions immediately. " +
	"The Vault credentialed policy still has this path until re-synced: " +
	"run `oberth install --install-secretstore --upgrade` to remove it from the Vault policy."

type APIConfig struct {
	// Publisher force-syncs a green run to the upstream on request. Supplied by
	// the scheduler, which owns the durable outbox; the API only forwards the
	// request so there is one publication path rather than two.
	Publisher              func(context.Context, string) error
	Runs                   RunResolver
	History                RunHistory
	Repositories           RepositoryReader
	Issues                 IssueRepository
	Promotions             PromotionRepository
	PromotionRuns          PromotionRunStore
	Enqueues               EnqueueObserver
	Git                    DeliveryGit
	Refs                   RefResolver
	Logs                   LogStore
	Artifacts              ArtifactStore
	Auditor                Auditor
	Health                 Health
	Signals                *Signals
	MaximumWait            time.Duration
	MutationGate           func(context.Context) error
	PromotionWorkspaceRoot string
	RemoveWorkspace        func(string) error
	SecretAccess           SecretAccessStore
	SecretAccessReconciler *AccessReconciler
	RepositoryRemover      RepositoryRemover
	RemoveGitCache         func(string) error
}

type API struct {
	runs                   RunResolver
	history                RunHistory
	repositories           RepositoryReader
	issues                 IssueRepository
	promotions             PromotionRepository
	promotionRuns          PromotionRunStore
	enqueues               EnqueueObserver
	git                    DeliveryGit
	logs                   LogStore
	artifacts              ArtifactStore
	refs                   RefResolver
	auditor                Auditor
	health                 Health
	signals                *Signals
	maximumWait            time.Duration
	mutationGate           func(context.Context) error
	promotionWorkspaceRoot string
	workspaces             *workspaceLifecycle
	secretAccess           SecretAccessStore
	secretAccessReconciler *AccessReconciler
	repositoryRemover      RepositoryRemover
	removeGitCache         func(string) error
	publisher              func(context.Context, string) error
}

func NewAPI(config APIConfig) (*API, error) {
	if config.Runs == nil {
		return nil, errors.New("service: run resolver is required")
	}
	maximumWait := config.MaximumWait
	if maximumWait <= 0 || maximumWait > defaultMaximumWait {
		maximumWait = defaultMaximumWait
	}
	promotionWorkspaceRoot := config.PromotionWorkspaceRoot
	if strings.TrimSpace(promotionWorkspaceRoot) == "" {
		promotionWorkspaceRoot = "/data/work"
	}
	promotionWorkspaceRoot = filepath.Clean(promotionWorkspaceRoot)
	var workspaces *workspaceLifecycle
	if config.Promotions != nil {
		var err error
		workspaces, err = newWorkspaceLifecycle(promotionWorkspaceRoot, config.Promotions, config.RemoveWorkspace)
		if err != nil {
			return nil, err
		}
	}
	signals := config.Signals
	if signals == nil {
		signals = NewSignals()
	}
	mutationGate := config.MutationGate
	if mutationGate == nil {
		mutationGate = func(context.Context) error { return nil }
	}
	return &API{
		runs: config.Runs, history: config.History, repositories: config.Repositories,
		issues: config.Issues, promotions: config.Promotions, promotionRuns: config.PromotionRuns,
		enqueues: config.Enqueues, git: config.Git, refs: config.Refs, logs: config.Logs, artifacts: config.Artifacts, auditor: config.Auditor,
		health: config.Health, signals: signals, maximumWait: maximumWait, mutationGate: mutationGate,
		promotionWorkspaceRoot: promotionWorkspaceRoot, workspaces: workspaces,
		secretAccess: config.SecretAccess, secretAccessReconciler: config.SecretAccessReconciler,
		repositoryRemover: config.RepositoryRemover, removeGitCache: config.RemoveGitCache,
		publisher: config.Publisher,
	}, nil
}

func (service *API) CallTool(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (any, error) {
	if strings.TrimSpace(actor.Identity) == "" {
		return nil, errors.New("service: acting uplink identity is required")
	}
	if mutatingTool(name) {
		if err := service.requireMutation(ctx); err != nil {
			return nil, err
		}
	}
	switch name {
	case "status":
		var arguments struct {
			Repo string `json:"repo"`
			Ref  string `json:"ref"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if strings.TrimSpace(arguments.Ref) == "" {
			return nil, fmt.Errorf("%w: ref is required", ErrInvalidInput)
		}
		return service.status(ctx, arguments.Repo, arguments.Ref, actor.Identity)
	case "logs":
		var arguments struct {
			Repo    string `json:"repo"`
			SHA     string `json:"sha"`
			Step    string `json:"step"`
			Pattern string `json:"pattern"`
			Context int    `json:"context"`
			Offset  int    `json:"offset"`
			Limit   int    `json:"limit"`
			Tail    bool   `json:"tail"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		filter, err := toolLogFilter(arguments.Pattern, arguments.Context, arguments.Offset, arguments.Limit, arguments.Tail)
		if err != nil {
			return nil, err
		}
		return service.logsFor(ctx, arguments.Repo, arguments.SHA, arguments.Step, filter)
	case "run_get":
		var arguments struct {
			ID string `json:"id"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if strings.TrimSpace(arguments.ID) == "" {
			return nil, fmt.Errorf("%w: run ID is required", ErrInvalidInput)
		}
		return service.Run(ctx, actor, arguments.ID)
	case "run_logs":
		var arguments struct {
			ID      string `json:"id"`
			Burn    string `json:"burn"`
			Step    string `json:"step"`
			Pattern string `json:"pattern"`
			Context int    `json:"context"`
			Offset  int    `json:"offset"`
			Limit   int    `json:"limit"`
			Tail    bool   `json:"tail"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if strings.TrimSpace(arguments.ID) == "" || strings.TrimSpace(arguments.Burn) == "" || strings.TrimSpace(arguments.Step) == "" {
			return nil, fmt.Errorf("%w: run ID, burn, and step are required", ErrInvalidInput)
		}
		filter, err := toolLogFilter(arguments.Pattern, arguments.Context, arguments.Offset, arguments.Limit, arguments.Tail)
		if err != nil {
			return nil, err
		}
		return service.RunLogFiltered(ctx, actor, arguments.ID, arguments.Burn, arguments.Step, filter)
	case "artifacts":
		var arguments struct {
			ID string `json:"id"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.RunArtifacts(ctx, actor, arguments.ID)
	case "artifact_get":
		var arguments struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Pattern string `json:"pattern"`
			Context int    `json:"context"`
			Offset  int    `json:"offset"`
			Limit   int    `json:"limit"`
			Tail    bool   `json:"tail"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if strings.TrimSpace(arguments.ID) == "" || strings.TrimSpace(arguments.Name) == "" {
			return nil, fmt.Errorf("%w: run ID and artifact name are required", ErrInvalidInput)
		}
		filter, err := toolLogFilter(arguments.Pattern, arguments.Context, arguments.Offset, arguments.Limit, arguments.Tail)
		if err != nil {
			return nil, err
		}
		return service.RunArtifact(ctx, actor, arguments.ID, arguments.Name, filter)
	case "wait":
		var arguments struct {
			Repo    string `json:"repo"`
			SHA     string `json:"sha"`
			Trigger string `json:"trigger"`
			Timeout int    `json:"timeout"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.waitRun(ctx, arguments.Repo, arguments.SHA, arguments.Trigger, arguments.Timeout, actor.Identity)
	case "sync":
		var arguments struct {
			Repo   string `json:"repo"`
			SHA    string `json:"sha"`
			Branch string `json:"branch"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.sync(ctx, actor, arguments.Repo, arguments.SHA, arguments.Branch)
	case "promote":
		var arguments struct {
			Repo   string `json:"repo"`
			SHA    string `json:"sha"`
			Branch string `json:"branch"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		promotion, err := service.promote(ctx, actor, arguments.Repo, arguments.SHA, arguments.Branch)
		if err != nil {
			return nil, err
		}
		return api.PromoteResponse{ID: promotion.ID}, nil
	case "promote_status":
		var arguments struct {
			ID      string `json:"id"`
			Timeout int    `json:"timeout"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.waitPromotion(ctx, arguments.ID, arguments.Timeout)
	case "issue_create":
		var arguments struct {
			Title string  `json:"title"`
			Body  *string `json:"body"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if arguments.Body == nil {
			return nil, fmt.Errorf("%w: issue body is required", ErrInvalidInput)
		}
		issue, err := service.issueCreate(ctx, actor, arguments.Title, *arguments.Body)
		if err != nil {
			return nil, err
		}
		return api.IssueCreateResponse{ID: issue.ID}, nil
	case "issue_get":
		var arguments struct {
			ID int64 `json:"id"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		issue, err := service.issueGet(ctx, actor, arguments.ID)
		if err != nil {
			return nil, err
		}
		return wireIssue(issue), nil
	case "issue_update":
		var arguments struct {
			ID    int64   `json:"id"`
			Title *string `json:"title"`
			Body  *string `json:"body"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		issue, err := service.issueUpdate(ctx, actor, arguments.ID, arguments.Title, arguments.Body)
		if err != nil {
			return nil, err
		}
		return wireIssue(issue), nil
	case "issue_close":
		var arguments struct {
			ID int64 `json:"id"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		issue, err := service.issueClose(ctx, actor, arguments.ID)
		if err != nil {
			return nil, err
		}
		return wireIssue(issue), nil
	case "issue_delete":
		var arguments struct {
			ID int64 `json:"id"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return nil, service.issueDelete(ctx, actor, arguments.ID)
	case "issue_list":
		var arguments struct {
			Before int64 `json:"before"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		page, err := service.issueList(ctx, "", "", "", arguments.Before, maximumIssuePage)
		if err != nil {
			return nil, err
		}
		return wireIssueList(page), nil
	case "issue_lock":
		var arguments struct {
			ID int64 `json:"id"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		if service.issues == nil {
			return nil, fmt.Errorf("%w: issues", ErrUnavailable)
		}
		lock, err := service.issues.AcquireIssueLock(ctx, arguments.ID, actor.Identity)
		if err != nil {
			return nil, err
		}
		return wireIssueLock(lock), nil
	case "access_list":
		var arguments struct {
			Repo    string `json:"repo"`
			Revoked bool   `json:"revoked"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.accessList(ctx, arguments.Repo, arguments.Revoked)
	case "access_allow":
		var arguments struct {
			Repo   string `json:"repo"`
			Step   string `json:"step"`
			Secret string `json:"secret"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.accessAllow(ctx, actor, arguments.Repo, arguments.Step, arguments.Secret)
	case "access_revoke":
		var arguments struct {
			Repo   string `json:"repo"`
			Step   string `json:"step"`
			Secret string `json:"secret"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.accessRevoke(ctx, actor, arguments.Repo, arguments.Step, arguments.Secret)
	case "repo_list":
		return service.Repositories(ctx, actor)
	case "repo_remove":
		var arguments struct {
			Repo string `json:"repo"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.repoRemove(ctx, actor, arguments.Repo)
	case "run_list":
		var arguments struct {
			Repo  string `json:"repo"`
			Ref   string `json:"ref"`
			Limit int    `json:"limit"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		limit := arguments.Limit
		if limit <= 0 {
			limit = 50
		}
		if limit > 200 {
			limit = 200
		}
		return service.Runs(ctx, actor, api.RunFilter{Repository: arguments.Repo, Ref: arguments.Ref, Limit: limit})
	case "system_status":
		return service.Status(ctx, actor)
	default:
		return nil, fmt.Errorf("%w: unknown tool %q", ErrInvalidInput, name)
	}
}

func (service *API) Runs(ctx context.Context, _ api.Actor, filter api.RunFilter) (any, error) {
	if service.history == nil {
		return nil, fmt.Errorf("%w: run history", ErrUnavailable)
	}
	storeFilter := model.RunListFilter{Ref: filter.Ref, Limit: filter.Limit}
	if strings.TrimSpace(filter.Repository) != "" {
		repository, err := service.runs.RepositoryByName(ctx, filter.Repository)
		if err != nil {
			return nil, err
		}
		storeFilter.RepoID = repository.ID
	}
	return service.history.ListRecentRuns(ctx, storeFilter)
}

// Run is the read-only dashboard view of one exact run: the durable record,
// its recorded step results, and the owning repository. Unlike the MCP status
// tool it resolves by run ID, never renews an issue lock, and has no other
// side effects, so an idle dashboard cannot hold agent coordination state.
func (service *API) Run(ctx context.Context, _ api.Actor, id string) (any, error) {
	if service.history == nil {
		return nil, fmt.Errorf("%w: run history", ErrUnavailable)
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: run ID is required", ErrInvalidInput)
	}
	run, err := service.history.Run(ctx, id)
	if err != nil {
		return nil, err
	}
	steps, err := service.stepsForRun(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("load run steps: %w", err)
	}
	response := RunDetailResponse{Run: run, Steps: steps}
	if service.repositories != nil {
		repository, repositoryErr := service.repositories.Repository(ctx, run.RepoID)
		if repositoryErr != nil && !errors.Is(repositoryErr, store.ErrNotFound) {
			return nil, fmt.Errorf("load run repository: %w", repositoryErr)
		}
		if repositoryErr == nil {
			response.Repository = repository
		}
	}
	return response, nil
}

// RunLog is the read-only dashboard view of one known step's bounded redacted
// log. Active runs authorize against their progress journal and read an
// in-memory index snapshot; terminal runs authorize against durable results
// and their finalized retained-log index.
func (service *API) RunLog(ctx context.Context, actor api.Actor, id, burn, step string) (any, error) {
	return service.RunLogFiltered(ctx, actor, id, burn, step, runlog.Filter{})
}

func (service *API) RunLogFiltered(ctx context.Context, _ api.Actor, id, burn, step string, filter runlog.Filter) (any, error) {
	if service.logs == nil || service.history == nil {
		return nil, fmt.Errorf("%w: logs", ErrUnavailable)
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(burn) == "" || strings.TrimSpace(step) == "" {
		return nil, fmt.Errorf("%w: run ID, burn, and step are required", ErrInvalidInput)
	}
	run, err := service.history.Run(ctx, id)
	if err != nil {
		return nil, err
	}
	steps, err := service.stepsForRun(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("load run steps: %w", err)
	}
	recorded := false
	for _, result := range steps {
		if result.Burn == burn && result.Step == step {
			recorded = true
			break
		}
	}
	if !recorded {
		return nil, fmt.Errorf("service: no log range for burn %q step %q", burn, step)
	}
	body, meta, err := service.readStepLogFiltered(run, burn, step, filter)
	if err != nil {
		return nil, fmt.Errorf("read bounded run log: %w", err)
	}
	return newLogResponse(run.ID, burn, step, body, meta), nil
}

// maxLiveLogChunk bounds one live-log poll response. 256 KiB keeps a fast
// producer from turning a dashboard poll into a multi-megabyte transfer while
// still draining bursts within a couple of poll cycles.
const maxLiveLogChunk = 256 << 10

// RunLogLive is the read-only polled live view of a run's redacted log
// stream. The run's terminal state is resolved BEFORE the chunk is read, so
// Done=true can never be reported while unread bytes remain: a run finishing
// between the two reads surfaces as one more running poll, and the client's
// next call drains the remainder with Done set. The underlying file receives
// only the secret-masked stream (masking happens in the Job controller before
// any byte reaches the log writer), so this view exposes exactly what the
// retained per-step view exposes — earlier.
func (service *API) RunLogLive(ctx context.Context, _ api.Actor, id string, offset int64) (any, error) {
	if service.logs == nil || service.history == nil {
		return nil, fmt.Errorf("%w: logs", ErrUnavailable)
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("%w: run ID is required", ErrInvalidInput)
	}
	run, err := service.history.Run(ctx, id)
	if err != nil {
		return nil, err
	}
	terminal := run.Status.Terminal()
	response := LiveLogResponse{RunID: run.ID, Status: string(run.Status)}
	chunk, next, size, err := service.logs.ReadFrom(run.ID, offset, maxLiveLogChunk)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The log file appears when the Job starts streaming; a queued or
			// just-admitted run legitimately has none yet. A terminal run with
			// no file is genuinely done.
			response.Done = terminal
			return response, nil
		}
		return nil, fmt.Errorf("read live run log: %w", err)
	}
	response.Chunk = string(chunk)
	response.Offset = next
	response.Size = size
	// Done is true only when the run is terminal AND the read has reached
	// the end of the file. This prevents reporting done while unread bytes
	// remain in a multi-chunk terminal log.
	response.Done = terminal && next >= size
	return response, nil
}

// PublishRun forwards an on-demand publication request to the scheduler.
//
// Present so the dashboard can offer "push, then open a pull request" when the
// server runs with --publish-on-green=false. When no publisher is wired the
// server is publishing automatically, so the request is meaningless rather than
// merely unavailable.
func (service *API) PublishRun(ctx context.Context, _ api.Actor, id string) error {
	if service.publisher == nil {
		return fmt.Errorf("%w: on-demand publication is off; this server publishes every green run automatically", ErrInvalidInput)
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: run ID is required", ErrInvalidInput)
	}
	return service.publisher(ctx, id)
}

func (service *API) Repositories(ctx context.Context, _ api.Actor) (any, error) {
	var repos []model.Repository
	var err error
	if service.repositories != nil {
		repos, err = service.repositories.ListRepositories(ctx)
	} else {
		repos, err = service.runs.ListRepositories(ctx)
	}
	if err != nil {
		return nil, err
	}
	// Fetch the latest runs per repo so the dashboard never derives per-repo
	// status from a global top-N window that silently drops quiet repos.
	var runsByRepo map[int64][]model.Run
	if service.history != nil {
		runs, historyErr := service.history.ListLatestRunsPerRepo(ctx, 12)
		if historyErr == nil {
			runsByRepo = make(map[int64][]model.Run, len(repos))
			for _, run := range runs {
				runsByRepo[run.RepoID] = append(runsByRepo[run.RepoID], run)
			}
		}
	}
	views := make([]RepoView, len(repos))
	for i, repo := range repos {
		views[i] = RepoView{Repository: repo, LatestRuns: runsByRepo[repo.ID]}
	}
	return views, nil
}

func (service *API) Issues(ctx context.Context, _ api.Actor, filter api.IssueFilter) (any, error) {
	return service.issueList(ctx, filter.Repository, filter.Kind, filter.State, int64(filter.BeforeID), filter.Limit)
}

func (service *API) Status(ctx context.Context, _ api.Actor) (any, error) {
	if service.health == nil {
		return nil, fmt.Errorf("%w: status", ErrUnavailable)
	}
	return service.health.Status(ctx)
}

func (service *API) Ready(ctx context.Context) error {
	if service.health == nil {
		return fmt.Errorf("%w: readiness", ErrUnavailable)
	}
	return service.health.Ready(ctx)
}

func (service *API) status(ctx context.Context, repositoryName, selector, actor string) (StatusResponse, error) {
	repository, run, err := service.resolveRun(ctx, repositoryName, selector)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) && service.refs != nil {
			return service.statusRefWithoutRun(ctx, repositoryName, selector, actor)
		}
		return StatusResponse{}, err
	}
	response := StatusResponse{
		Repository: repository, Run: run, Repo: repository.Name, Ref: run.Ref,
		SHA: run.SHA, RunID: run.ID, Status: wireRunStatus(run.Status), Burns: map[string]string{},
	}
	if service.history != nil {
		response.Steps, err = service.stepsForRun(ctx, run)
		if err != nil {
			return StatusResponse{}, fmt.Errorf("load run steps: %w", err)
		}
		response.Burns, response.ExitCode = wireBurns(response.Steps, run)
	}
	response.FailedStep = run.FailedStep
	if service.issues != nil && run.RefKind == model.RefBranch {
		issue, issueErr := service.issues.OpenCIIssue(ctx, repository.ID, run.Ref)
		if issueErr != nil && !errors.Is(issueErr, store.ErrNotFound) {
			return StatusResponse{}, fmt.Errorf("load run issue: %w", issueErr)
		}
		if issueErr == nil {
			issueID := issue.ID
			response.Issue = &issueID
		}
	}
	return response, nil
}

// statusRefWithoutRun resolves a selector as a branch name in the bare Git
// cache when no run exists for that ref name. After resolving the SHA it also
// tries a SHA-based run lookup, which catches promotion runs whose ref is
// "promotion/<target>/<prefix>" rather than the bare branch name.
func (service *API) statusRefWithoutRun(ctx context.Context, repositoryName, selector, actor string) (StatusResponse, error) {
	if strings.TrimSpace(repositoryName) != "" {
		repository, err := service.runs.RepositoryByName(ctx, repositoryName)
		if err != nil {
			return StatusResponse{}, err
		}
		sha, err := service.refs.RefSHA(ctx, repository.Name, selector)
		if err != nil {
			return StatusResponse{}, fmt.Errorf("%w: run selector %q", store.ErrNotFound, selector)
		}
		run, runErr := service.runs.ResolveRun(ctx, repository.ID, sha)
		if runErr == nil {
			return service.statusFromRun(ctx, repository, run, selector, actor)
		}
		if !errors.Is(runErr, store.ErrNotFound) {
			return StatusResponse{}, runErr
		}
		return StatusResponse{
			Repository: repository, Repo: repository.Name, Ref: selector,
			SHA: sha, Status: "no-runs", Burns: map[string]string{},
		}, nil
	}
	repositories, err := service.runs.ListRepositories(ctx)
	if err != nil {
		return StatusResponse{}, err
	}
	var matched model.Repository
	var matchedSHA string
	for _, repository := range repositories {
		sha, refErr := service.refs.RefSHA(ctx, repository.Name, selector)
		if refErr != nil {
			continue
		}
		if matched.ID != 0 {
			return StatusResponse{}, fmt.Errorf("%w: %q", ErrAmbiguousRepository, selector)
		}
		matched, matchedSHA = repository, sha
	}
	if matched.ID == 0 {
		return StatusResponse{}, fmt.Errorf("%w: run selector %q", store.ErrNotFound, selector)
	}
	run, runErr := service.runs.ResolveRun(ctx, matched.ID, matchedSHA)
	if runErr == nil {
		return service.statusFromRun(ctx, matched, run, selector, actor)
	}
	if !errors.Is(runErr, store.ErrNotFound) {
		return StatusResponse{}, runErr
	}
	return StatusResponse{
		Repository: matched, Repo: matched.Name, Ref: selector,
		SHA: matchedSHA, Status: "no-runs", Burns: map[string]string{},
	}, nil
}

// statusFromRun builds a full StatusResponse from an already-resolved
// repository and run, overriding the response ref with the caller's original
// selector (so "status main" shows ref "main" even when the run's own ref is
// a promotion ref like "promotion/main/abcdef012345").
func (service *API) statusFromRun(ctx context.Context, repository model.Repository, run model.Run, ref, actor string) (StatusResponse, error) {
	response := StatusResponse{
		Repository: repository, Run: run, Repo: repository.Name, Ref: ref,
		SHA: run.SHA, RunID: run.ID, Status: wireRunStatus(run.Status), Burns: map[string]string{},
	}
	if service.history != nil {
		var err error
		response.Steps, err = service.stepsForRun(ctx, run)
		if err != nil {
			return StatusResponse{}, fmt.Errorf("load run steps: %w", err)
		}
		response.Burns, response.ExitCode = wireBurns(response.Steps, run)
	}
	response.FailedStep = run.FailedStep
	if service.issues != nil && run.RefKind == model.RefBranch {
		issue, issueErr := service.issues.OpenCIIssue(ctx, repository.ID, run.Ref)
		if issueErr != nil && !errors.Is(issueErr, store.ErrNotFound) {
			return StatusResponse{}, fmt.Errorf("load run issue: %w", issueErr)
		}
		if issueErr == nil {
			issueID := issue.ID
			response.Issue = &issueID
		}
	}
	return response, nil
}

func wireRunStatus(status model.RunStatus) string {
	if status == model.RunPassed {
		return "delivered"
	}
	return string(status)
}

// burnPrecedence resolves a burn to one word from the states of its steps.
// Highest wins, so the answer does not depend on the order the steps arrive
// in: any failure makes the burn failed, otherwise the least-advanced step
// that still has work ahead of it names the burn, and only a burn whose steps
// have all finished reports a finished state.
//
// pending sits between queued and passed for exactly that reason. A burn with
// one passed step and one the run never reached is not a passed burn, and
// before the plan existed it could not say so -- the unreached step was absent
// from the list entirely, so the burn read "passed".
var burnPrecedence = map[string]int{"-": 0, "passed": 1, "pending": 2, "queued": 3, "running": 4, "failed": 5}

func wireBurns(steps []model.StepResult, run model.Run) (map[string]string, *int) {
	burns := make(map[string]string)
	var exitCode *int
	for _, step := range steps {
		state := ""
		switch step.Status {
		case model.StepFailed, model.StepTimedOut:
			state = "failed"
			if exitCode == nil && matchesFailedStep(step, run) {
				value := step.ExitCode
				exitCode = &value
			}
		case model.StepRunning:
			state = "running"
		case model.StepQueued:
			state = "queued"
		case model.StepPending:
			state = "pending"
		case model.StepPassed:
			state = "passed"
		case model.StepSkipped:
			state = "-"
		default:
			continue
		}
		if current, seen := burns[step.Burn]; !seen || burnPrecedence[state] > burnPrecedence[current] {
			burns[step.Burn] = state
		}
	}
	return burns, exitCode
}

// matchesFailedStep returns true when the given step result matches the run's
// recorded failed step identity. When both FailedBurn and FailedStep are
// populated, both must match; when only one is populated, it alone decides.
// When neither is populated, the first failed step wins (the caller already
// checks exitCode == nil).
func matchesFailedStep(step model.StepResult, run model.Run) bool {
	burnRecorded := run.FailedBurn != ""
	stepRecorded := run.FailedStep != ""
	if !burnRecorded && !stepRecorded {
		return true // no identity recorded; take the first failed step
	}
	burnMatch := !burnRecorded || step.Burn == run.FailedBurn
	stepMatch := !stepRecorded || step.Step == run.FailedStep
	return burnMatch && stepMatch
}

// stepsForRun assembles the run's step list from the three records that each
// know a different amount about it: the plan knows which steps exist, the
// progress journal knows which ones started, and the durable results know how
// they ended.
//
// All three are read for terminal runs too, not just active ones. A run that
// was interrupted mid-flight records no results at all -- persistSteps only
// ever runs from a completed Job's result -- so reading only the store gave it
// an empty step list, discarding both the plan it was admitted with and the
// progress it had visibly made. Its honest record is the plan, with the steps
// that reached a state filled in.
func (service *API) stepsForRun(ctx context.Context, run model.Run) ([]model.StepResult, error) {
	steps, err := service.history.StepResults(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	if service.logs == nil {
		return steps, nil
	}
	planned, err := service.logs.StepPlan(run.ID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// A run submitted before plans existed, or by an engine that cannot
		// enumerate its document, simply has none.
		planned = nil
	}
	events, err := service.logs.StepProgress(run.ID)
	switch {
	case errors.Is(err, os.ErrNotExist):
		events = nil
	case err != nil && run.Status.Active():
		// While a run is live the journal is the primary record of in-flight
		// state, so failing to read it is a real failure.
		return nil, fmt.Errorf("load active step progress: %w", err)
	case err != nil:
		// For a terminal run the journal only supplements durable results.
		// Degrading to those beats failing the request for a finished run.
		events = nil
	}
	if len(planned) == 0 && len(events) == 0 {
		return steps, nil
	}
	return mergeSteps(run.ID, planned, events, steps), nil
}

// mergeSteps overlays the three records in increasing order of authority, and
// only ever forward: a source may advance a step's state, never walk it back.
// That is what lets a seeded plan entry be replaced by a live observation and
// then by a durable result without any of them needing to know the others
// exist, and what keeps the step count equal to the planned count for the
// whole life of the run instead of however many steps happened to start.
func mergeSteps(runID string, planned []runprogress.PlannedStep, events []runprogress.Event, persisted []model.StepResult) []model.StepResult {
	order := make(map[string]int, len(planned))
	for _, step := range planned {
		order[step.Burn+"\x00"+step.Step] = step.Ordinal
	}
	merged := make([]model.StepResult, 0, len(planned)+len(events)+len(persisted))
	position := make(map[string]int, cap(merged))
	advance := func(candidate model.StepResult) {
		key := candidate.Burn + "\x00" + candidate.Step
		if ordinal, isPlanned := order[key]; isPlanned {
			candidate.Ordinal = ordinal
		} else if len(order) != 0 {
			// Observed but never planned. It sorts after every planned step
			// rather than displacing one, keeping the plan's own order intact.
			candidate.Ordinal += len(order)
		}
		index, seen := position[key]
		if !seen {
			position[key] = len(merged)
			merged = append(merged, candidate)
			return
		}
		if candidate.Status.Progress() >= merged[index].Status.Progress() {
			merged[index] = candidate
		}
	}
	for _, step := range planned {
		advance(model.StepResult{
			RunID: runID, Burn: step.Burn, Step: step.Step,
			Ordinal: step.Ordinal, Status: model.StepPending,
		})
	}
	for _, event := range events {
		advance(progressStepResult(runID, event))
	}
	for _, result := range persisted {
		advance(result)
	}
	sort.SliceStable(merged, func(left, right int) bool {
		if merged[left].Ordinal != merged[right].Ordinal {
			return merged[left].Ordinal < merged[right].Ordinal
		}
		if merged[left].Burn != merged[right].Burn {
			return merged[left].Burn < merged[right].Burn
		}
		return merged[left].Step < merged[right].Step
	})
	return merged
}

func progressStepResult(runID string, event runprogress.Event) model.StepResult {
	return model.StepResult{
		RunID: runID, Burn: event.Burn, Step: event.Step, Ordinal: event.Ordinal,
		Status: model.StepStatus(event.Status), ExitCode: event.ExitCode,
		DeclaredSize: event.DeclaredSize.Effective(), StartedAt: event.StartedAt, FinishedAt: event.FinishedAt,
	}
}

func toolLogFilter(pattern string, context, offset, limit int, tail bool) (runlog.Filter, error) {
	if context < 0 || offset < 0 || limit < 0 {
		return runlog.Filter{}, fmt.Errorf("%w: log context, offset, and limit must not be negative", ErrInvalidInput)
	}
	return runlog.Filter{Pattern: pattern, Context: context, Offset: offset, Limit: limit, Tail: tail}, nil
}

func (service *API) logsFor(ctx context.Context, repositoryName, selector, wanted string, filter runlog.Filter) (LogResponse, error) {
	if service.logs == nil || service.history == nil {
		return LogResponse{}, fmt.Errorf("%w: logs", ErrUnavailable)
	}
	if !validSHASelector(selector) || strings.TrimSpace(wanted) == "" {
		return LogResponse{}, fmt.Errorf("%w: SHA and step are required", ErrInvalidInput)
	}
	_, run, err := service.resolveRun(ctx, repositoryName, selector)
	if err != nil {
		return LogResponse{}, err
	}
	steps, err := service.stepsForRun(ctx, run)
	if err != nil {
		return LogResponse{}, fmt.Errorf("load run steps: %w", err)
	}
	burn, step, err := selectLogRange(steps, wanted)
	if err != nil {
		return LogResponse{}, err
	}
	body, meta, err := service.readStepLogFiltered(run, burn, step, filter)
	if err != nil {
		return LogResponse{}, fmt.Errorf("read bounded run log: %w", err)
	}
	return newLogResponse(run.ID, burn, step, body, meta), nil
}

func newLogResponse(runID, burn, step string, body []byte, meta runlog.Meta) LogResponse {
	return LogResponse{
		RunID: runID, Burn: burn, Step: step, Output: string(body),
		TotalLines: meta.TotalLines, MatchedLines: meta.MatchedLines,
		ReturnedLines: meta.ReturnedLines, Truncated: meta.Truncated, Bytes: meta.Bytes,
	}
}

func (service *API) readStepLogFiltered(run model.Run, burn, step string, filter runlog.Filter) ([]byte, runlog.Meta, error) {
	if run.Status.Active() {
		return service.logs.ReadActiveFiltered(run.ID, burn, step, filter)
	}
	return service.logs.ReadFiltered(run.ID, burn, step, filter)
}

func selectLogRange(results []model.StepResult, wanted string) (string, string, error) {
	selectedBurn := ""
	for _, result := range results {
		if result.Step != wanted {
			continue
		}
		if selectedBurn != "" {
			return "", "", fmt.Errorf("%w: step %q is ambiguous across burns", ErrInvalidInput, wanted)
		}
		selectedBurn = result.Burn
	}
	if selectedBurn == "" {
		return "", "", fmt.Errorf("service: no log range for step %q", wanted)
	}
	return selectedBurn, wanted, nil
}

func (service *API) waitRun(ctx context.Context, repositoryName, selector, trigger string, requestedSeconds int, actor string) (WaitResponse, error) {
	if !validSHASelector(selector) {
		return WaitResponse{}, fmt.Errorf("%w: full or short SHA is required", ErrInvalidInput)
	}
	trigger = strings.TrimSpace(trigger)
	duration, err := service.waitDuration(requestedSeconds)
	if err != nil {
		return WaitResponse{}, err
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		status, err := service.status(ctx, repositoryName, selector, actor)
		if err != nil {
			return WaitResponse{}, err
		}
		// When a trigger filter is active, skip terminal results from runs
		// whose trigger does not match. A tag push creates a release run
		// with the same commit SHA as the branch run; without this guard,
		// wait returns the already-terminal branch run immediately instead
		// of waiting for the release run to appear and finish.
		triggerMatch := trigger == "" || status.Run.Trigger == trigger
		if triggerMatch && status.Run.Status.Terminal() {
			return WaitResponse{StatusResponse: status}, nil
		}
		changed := service.signals.Run(status.Run.ID)
		// Re-read after subscribing so a transition between the first read and
		// channel lookup cannot be lost.
		status, err = service.status(ctx, repositoryName, selector, actor)
		if err != nil {
			return WaitResponse{}, err
		}
		triggerMatch = trigger == "" || status.Run.Trigger == trigger
		if triggerMatch && status.Run.Status.Terminal() {
			return WaitResponse{StatusResponse: status}, nil
		}
		select {
		case <-ctx.Done():
			return WaitResponse{}, ctx.Err()
		case <-timer.C:
			return WaitResponse{StatusResponse: status, StillRunning: true}, nil
		case <-changed:
		}
	}
}

func validSHASelector(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 7 || len(value) > 64 {
		return false
	}
	for _, character := range strings.ToLower(value) {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func (service *API) sync(ctx context.Context, actor api.Actor, repositoryName, selector, branch string) (model.Run, error) {
	if service.git == nil || service.auditor == nil {
		return model.Run{}, fmt.Errorf("%w: sync", ErrUnavailable)
	}
	if !validOID(selector) {
		return model.Run{}, fmt.Errorf("%w: sync requires a full SHA", ErrInvalidInput)
	}
	repository, run, err := service.resolveRun(ctx, repositoryName, selector)
	if err != nil {
		return model.Run{}, err
	}
	if run.RefKind != model.RefBranch {
		return model.Run{}, fmt.Errorf("%w: sync only accepts branch runs", ErrInvalidInput)
	}
	// Exclude promotion, release, and internal-trigger runs before any audit
	// or Git mutation. A commit shared across branches can resolve to a
	// promotion run with a synthetic RefBranch, which would rewind the wrong
	// real branch.
	trigger := strings.TrimSpace(run.Trigger)
	if trigger == "promotion" || trigger == "release" || trigger == "apply" || trigger == "plan" {
		return model.Run{}, fmt.Errorf("%w: sync does not accept %s runs", ErrInvalidInput, trigger)
	}
	// When an explicit branch is provided, validate the resolved run matches
	// it exactly. If the initial SHA-based resolution returned a run on a
	// different branch (because resolveRun picks the highest-queue-sequence
	// run), try ref-based resolution for the requested branch and verify
	// the SHA matches.
	if trimmedBranch := strings.TrimSpace(branch); trimmedBranch != "" {
		if run.Ref != trimmedBranch {
			// The SHA-resolved run is on a different branch. Try to find a
			// run on the requested branch whose SHA matches.
			branchRun, branchErr := service.runs.ResolveRun(ctx, repository.ID, trimmedBranch)
			if branchErr != nil || branchRun.SHA != run.SHA {
				return model.Run{}, fmt.Errorf("%w: no run for SHA %s on branch %q", ErrInvalidInput, selector, trimmedBranch)
			}
			run = branchRun
			// Re-check trigger exclusion for the branch-resolved run.
			branchTrigger := strings.TrimSpace(run.Trigger)
			if branchTrigger == "promotion" || branchTrigger == "release" || branchTrigger == "apply" || branchTrigger == "plan" {
				return model.Run{}, fmt.Errorf("%w: sync does not accept %s runs", ErrInvalidInput, branchTrigger)
			}
		}
	} else if resolver, ok := service.runs.(BranchRefResolver); ok {
		// No explicit branch — verify the SHA is not ambiguous across
		// branches. A bare-SHA sync of a commit with branch-trigger runs on
		// multiple distinct branches would silently resolve to the
		// highest-queue-sequence run and could force-push a branch the
		// caller did not intend.
		candidateRefs, refErr := resolver.DistinctBranchRefsForSHA(ctx, repository.ID, run.SHA)
		if refErr != nil {
			return model.Run{}, fmt.Errorf("check branch ambiguity: %w", refErr)
		}
		if len(candidateRefs) > 1 {
			return model.Run{}, fmt.Errorf("%w: SHA %s has branch-trigger runs on multiple branches (%s); pass the explicit branch argument to disambiguate",
				ErrInvalidInput, run.SHA, strings.Join(candidateRefs, ", "))
		}
	}
	if err := auditedGitMutation(ctx, service.auditor, actor.Identity, "sync.branch", "run", run.ID,
		map[string]any{"repo": repository.Name, "branch": run.Ref, "sha": run.SHA},
		func() error { return service.git.SyncBranch(ctx, repository.Name, run.Ref, run.SHA) }); err != nil {
		return model.Run{}, fmt.Errorf("sync branch: %w", err)
	}
	return run, nil
}

func (service *API) promote(ctx context.Context, actor api.Actor, repositoryName, selector, target string) (result model.Promotion, resultErr error) {
	if service.git == nil || service.promotions == nil || service.promotionRuns == nil || service.enqueues == nil || service.auditor == nil {
		return model.Promotion{}, fmt.Errorf("%w: promotion", ErrUnavailable)
	}
	if strings.TrimSpace(target) == "" {
		return model.Promotion{}, fmt.Errorf("%w: promotion target is required", ErrInvalidInput)
	}
	if !validOID(selector) {
		return model.Promotion{}, fmt.Errorf("%w: promotion requires a full SHA", ErrInvalidInput)
	}
	repository, candidate, err := service.resolveRun(ctx, repositoryName, selector)
	if err != nil {
		return model.Promotion{}, err
	}
	if candidate.RefKind != model.RefBranch || candidate.Status != model.RunPassed || !sameOID(candidate.TestedSHA, candidate.SHA) {
		return model.Promotion{}, errors.New("service: promotion source must be the exactly tested green branch commit")
	}
	promotion, err := service.promotions.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repository.ID, SourceBranch: candidate.Ref, SourceSHA: candidate.SHA,
		TargetRef: target, Actor: actor.Identity,
	})
	if err != nil {
		return model.Promotion{}, fmt.Errorf("admit promotion: %w", err)
	}
	if err := os.MkdirAll(service.promotionWorkspaceRoot, 0o750); err != nil {
		return service.failAdmittedPromotion(ctx, promotion, "create promotion workspace root: "+err.Error(), "")
	}
	workspaceParent := filepath.Join(service.promotionWorkspaceRoot, "promotion-"+promotion.ID)
	workspaceSource := filepath.Join(workspaceParent, "src")
	if !pathWithin(service.promotionWorkspaceRoot, workspaceParent) || !pathWithin(workspaceParent, workspaceSource) {
		return service.failAdmittedPromotion(ctx, promotion, "promotion workspace escaped its configured root", "")
	}
	if err := os.Mkdir(workspaceParent, 0o700); err != nil {
		return service.failAdmittedPromotion(ctx, promotion, "reserve promotion workspace: "+err.Error(), "")
	}
	defer func() {
		cleanupCtx, cancelCleanup := boundedWorkspaceCleanupContext(ctx)
		defer cancelCleanup()
		result = service.cleanupTerminalPromotionWorkspace(cleanupCtx, result)
	}()
	plan, err := service.git.PreparePromotion(ctx, repository.Name, candidate.SHA, target, workspaceSource)
	if err != nil {
		return service.failAdmittedPromotion(ctx, promotion, "prepare promotion: "+err.Error(), "")
	}
	if !validOID(plan.BaseSHA) || !validOID(plan.MergedSHA) {
		return service.failAdmittedPromotion(ctx, promotion, "promotion plan contains invalid object IDs", "")
	}
	planned, err := service.promotions.PlanPromotion(ctx, promotion.ID, plan.BaseSHA, plan.MergedSHA)
	if err != nil {
		return service.failAdmittedPromotion(ctx, promotion, "record promotion plan: "+err.Error(), "")
	}
	promotion = planned
	// The promotion.pending audit action is now recorded atomically inside
	// Store.PlanPromotion's own transaction — committed before PlanPromotion
	// returns and before any enqueue, so no competing queue wake can claim
	// unaudited work. A separate service-level auditPromotion call here
	// would duplicate the action.
	if plan.FastForward {
		publication, err := service.promotions.BeginPublication(ctx, model.PublicationSpec{
			RepoID: repository.ID, PromotionID: promotion.ID, RefKind: model.RefBranch, Ref: target,
			PreviousSHA: plan.BaseSHA, PreviousKnown: true,
			ResultSHA: plan.MergedSHA, Actor: actor.Identity,
		})
		if err != nil {
			return service.failAdmittedPromotion(ctx, promotion, "record fast-forward publication: "+err.Error(), "")
		}
		deliveryCtx, cancelDelivery := context.WithTimeout(context.WithoutCancel(ctx), promotionCompensationTimeout)
		finalization, err := service.enqueues.DeliverPublication(deliveryCtx, publication.ID)
		cancelDelivery()
		if err != nil {
			service.enqueues.NotifyPublicationRecovery(publication.ID)
			lookupCtx, cancelLookup := context.WithTimeout(context.WithoutCancel(ctx), promotionCompensationTimeout)
			current, lookupErr := service.promotions.Promotion(lookupCtx, promotion.ID)
			cancelLookup()
			if lookupErr == nil {
				current.Error = fmt.Sprintf("publication %s awaits recovery: %v", publication.ID, err)
				return current, nil
			}
			promotion.Error = errors.Join(err, lookupErr).Error()
			return promotion, nil
		}
		service.enqueues.NotifyQueue()
		service.signals.NotifyPromotion(promotion.ID)
		return finalization.Promotion, nil
	}

	enqueued, attached, err := service.promotionRuns.EnqueueAdmittedPromotionRun(ctx, model.RunSpec{
		RepoID: repository.ID, RefKind: model.RefBranch,
		Ref: promotionRunRef(target, plan.MergedSHA), SHA: plan.MergedSHA,
		Actor: actor.Identity, Trigger: "promotion", TestedSHA: plan.MergedSHA, BaseSHA: plan.BaseSHA,
	}, promotion.ID)
	if err != nil {
		return service.failAdmittedPromotion(ctx, promotion, "record merged promotion run: "+err.Error(), "")
	}
	promotion = attached
	if err := service.enqueues.AcceptEnqueue(ctx, enqueued); err != nil {
		return service.failAdmittedPromotion(ctx, promotion, err.Error(), enqueued.ID)
	}
	return promotion, nil
}

func (service *API) failAdmittedPromotion(ctx context.Context, promotion model.Promotion, reason, runID string) (model.Promotion, error) {
	// Admission is already durable. Compensation must outlive a disconnected or
	// timed-out caller, but it must still have a finite storage deadline.
	finalizeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promotionCompensationTimeout)
	defer cancel()
	if runID != "" {
		_, err := service.promotionRuns.FinishRun(finalizeCtx, runID, model.RunResult{
			Status: model.RunFailed, Phase: "promotion", Error: reason,
		})
		service.signals.NotifyRun(runID)
		service.signals.NotifyPromotion(promotion.ID)
		if err != nil {
			promotion.Error = errors.Join(errors.New(reason), err).Error()
			return promotion, nil
		}
		failed, err := service.promotions.Promotion(finalizeCtx, promotion.ID)
		if err != nil {
			promotion.Error = errors.Join(errors.New(reason), err).Error()
			return promotion, nil
		}
		return failed, nil
	}
	failed, err := service.promotions.FinishPromotion(finalizeCtx, promotion.ID, model.PromotionFailed, "", reason)
	service.signals.NotifyPromotion(promotion.ID)
	if err != nil {
		promotion.Error = errors.Join(errors.New(reason), err).Error()
		return promotion, nil
	}
	return failed, nil
}

func promotionRunRef(target, sha string) string {
	suffix := sha
	if len(sha) >= 12 {
		suffix = sha[:12]
	} else if sha == "" {
		suffix = "unknown"
	}
	return "promotion/" + strings.Trim(target, "/") + "/" + suffix
}

func (service *API) waitPromotion(ctx context.Context, id string, requestedSeconds int) (PromotionWaitResponse, error) {
	if service.promotions == nil || strings.TrimSpace(id) == "" {
		return PromotionWaitResponse{}, fmt.Errorf("%w: promotion ID is required", ErrInvalidInput)
	}
	duration, err := service.waitDuration(requestedSeconds)
	if err != nil {
		return PromotionWaitResponse{}, err
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		promotion, err := service.promotions.Promotion(ctx, id)
		if err != nil {
			return PromotionWaitResponse{}, err
		}
		if promotion.Status.Terminal() {
			return PromotionWaitResponse{Promotion: promotion}, nil
		}
		changed := service.signals.Promotion(id)
		promotion, err = service.promotions.Promotion(ctx, id)
		if err != nil {
			return PromotionWaitResponse{}, err
		}
		if promotion.Status.Terminal() {
			return PromotionWaitResponse{Promotion: promotion}, nil
		}
		select {
		case <-ctx.Done():
			return PromotionWaitResponse{}, ctx.Err()
		case <-timer.C:
			response := PromotionWaitResponse{Promotion: promotion}
			response.StillRunning = true
			return response, nil
		case <-changed:
		}
	}
}

func (service *API) issueCreate(ctx context.Context, actor api.Actor, title, body string) (model.Issue, error) {
	if service.issues == nil || strings.TrimSpace(title) == "" {
		return model.Issue{}, fmt.Errorf("%w: issue title is required", ErrInvalidInput)
	}
	return service.issues.CreateManualIssue(ctx, actor.Identity, model.ManualIssueSpec{Title: title, Body: body})
}

func (service *API) issueGet(ctx context.Context, _ api.Actor, id int64) (model.Issue, error) {
	if service.issues == nil || id <= 0 {
		return model.Issue{}, fmt.Errorf("%w: issue ID is required", ErrInvalidInput)
	}
	return service.issues.Issue(ctx, id)
}

func (service *API) issueUpdate(ctx context.Context, actor api.Actor, id int64, title, body *string) (model.Issue, error) {
	if service.issues == nil || id <= 0 || title == nil || body == nil {
		return model.Issue{}, fmt.Errorf("%w: issue ID, title, and body are required", ErrInvalidInput)
	}
	issue, err := service.issues.UpdateIssue(ctx, actor.Identity, id, model.IssuePatch{Title: title, Body: body})
	if err != nil {
		return model.Issue{}, err
	}
	return issue, nil
}

func (service *API) issueClose(ctx context.Context, actor api.Actor, id int64) (model.Issue, error) {
	if service.issues == nil || id <= 0 {
		return model.Issue{}, fmt.Errorf("%w: issue ID is required", ErrInvalidInput)
	}
	issue, err := service.issues.Issue(ctx, id)
	if err != nil {
		return model.Issue{}, err
	}
	if issue.Kind == model.IssueCI {
		issue, err = service.issues.CloseCIIssue(ctx, actor.Identity, issue.ID, "closed manually by "+actor.Identity)
	} else {
		closed := model.IssueClosed
		issue, err = service.issues.UpdateManualIssue(ctx, actor.Identity, id, model.IssuePatch{State: &closed})
	}
	if err != nil {
		return model.Issue{}, err
	}
	return issue, nil
}

func (service *API) issueDelete(ctx context.Context, actor api.Actor, id int64) error {
	if service.issues == nil || id <= 0 {
		return fmt.Errorf("%w: issue ID is required", ErrInvalidInput)
	}
	return service.issues.DeleteManualIssue(ctx, actor.Identity, id)
}

func (service *API) issueList(ctx context.Context, repositoryName, kind, state string, before int64, limit int) (model.IssuePage, error) {
	if service.issues == nil {
		return model.IssuePage{}, fmt.Errorf("%w: issues", ErrUnavailable)
	}
	filter := model.IssueListFilter{Kind: model.IssueKind(kind), State: model.IssueState(state), BeforeID: before, Limit: limit}
	if repositoryName != "" {
		repository, err := service.runs.RepositoryByName(ctx, repositoryName)
		if err != nil {
			return model.IssuePage{}, err
		}
		filter.RepoID = repository.ID
	}
	if filter.Kind != "" && !filter.Kind.Valid() {
		return model.IssuePage{}, fmt.Errorf("%w: issue kind", ErrInvalidInput)
	}
	if filter.State != "" && !filter.State.Valid() {
		return model.IssuePage{}, fmt.Errorf("%w: issue state", ErrInvalidInput)
	}
	if filter.BeforeID < 0 {
		return model.IssuePage{}, fmt.Errorf("%w: issue cursor", ErrInvalidInput)
	}
	if filter.Limit <= 0 || filter.Limit > maximumIssuePage {
		filter.Limit = maximumIssuePage
	}
	return service.issues.ListIssues(ctx, filter)
}

func wireIssue(issue model.Issue) api.IssueResponse {
	return api.IssueResponse{
		ID: issue.ID, Kind: string(issue.Kind), Title: issue.Title,
		Body: issue.Body, State: string(issue.State),
		RepoID: issue.RepoID, Branch: issue.Branch,
		Occurrences: int(issue.Occurrences),
		CIOrigin:    issue.CIOrigin, CIWorkID: issue.CIWorkID,
		CreatedAt: issue.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: issue.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func wireIssueList(page model.IssuePage) api.IssueListResponse {
	issues := make([]api.IssueListItem, len(page.Issues))
	for index, issue := range page.Issues {
		item := api.IssueListItem{
			ID:          issue.ID,
			State:       string(issue.State),
			Kind:        string(issue.Kind),
			Title:       issue.Title,
			RepoID:      issue.RepoID,
			Branch:      issue.Branch,
			Occurrences: int(issue.Occurrences),
			CIOrigin:    issue.CIOrigin,
			CIWorkID:    issue.CIWorkID,
			CreatedAt:   issue.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:   issue.UpdatedAt.UTC().Format(time.RFC3339),
		}
		issues[index] = item
	}
	return api.IssueListResponse{Issues: issues, NextBefore: page.NextBefore}
}

func wireIssueLock(lock model.IssueLock) api.IssueLockResponse {
	return api.IssueLockResponse{
		ID: lock.IssueID, Owner: lock.Owner,
		ExpiresAt: lock.ExpiresAt.UTC().Format(time.RFC3339Nano),
		Renewed:   true,
	}
}

func mutatingTool(name string) bool {
	switch name {
	case "sync", "promote", "issue_create", "issue_update", "issue_close", "issue_delete", "issue_lock",
		"access_allow", "access_revoke",
		"repo_remove":
		return true
	default:
		return false
	}
}

func (service *API) requireMutation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if service.mutationGate == nil {
		return nil
	}
	if err := service.mutationGate(ctx); err != nil {
		return fmt.Errorf("%w: audit integrity is unavailable: %w", ErrUnavailable, err)
	}
	return nil
}

func (service *API) resolveRun(ctx context.Context, repositoryName, selector string) (model.Repository, model.Run, error) {
	if strings.TrimSpace(selector) == "" {
		return model.Repository{}, model.Run{}, fmt.Errorf("%w: run selector is required", ErrInvalidInput)
	}
	if strings.TrimSpace(repositoryName) != "" {
		repository, err := service.runs.RepositoryByName(ctx, repositoryName)
		if err != nil {
			return model.Repository{}, model.Run{}, err
		}
		run, err := service.runs.ResolveRun(ctx, repository.ID, selector)
		return repository, run, err
	}
	repositories, err := service.runs.ListRepositories(ctx)
	if err != nil {
		return model.Repository{}, model.Run{}, err
	}
	var matchedRepository model.Repository
	var matchedRun model.Run
	for _, repository := range repositories {
		run, resolveErr := service.runs.ResolveRun(ctx, repository.ID, selector)
		if errors.Is(resolveErr, store.ErrNotFound) {
			continue
		}
		if resolveErr != nil {
			return model.Repository{}, model.Run{}, resolveErr
		}
		if matchedRepository.ID != 0 {
			return model.Repository{}, model.Run{}, fmt.Errorf("%w: %q", ErrAmbiguousRepository, selector)
		}
		matchedRepository, matchedRun = repository, run
	}
	if matchedRepository.ID == 0 {
		return model.Repository{}, model.Run{}, fmt.Errorf("%w: run selector %q", store.ErrNotFound, selector)
	}
	return matchedRepository, matchedRun, nil
}

func (service *API) waitDuration(seconds int) (time.Duration, error) {
	if seconds < 0 {
		return 0, fmt.Errorf("%w: timeout must not be negative", ErrInvalidInput)
	}
	if seconds == 0 {
		return service.maximumWait, nil
	}
	requested := time.Duration(seconds) * time.Second
	if requested > service.maximumWait {
		return 0, fmt.Errorf("%w: timeout exceeds maximum %s", ErrInvalidInput, service.maximumWait)
	}
	return requested, nil
}

// canonicalAccessRepo resolves any accepted repository spelling (bare,
// org/repo, upstream/org/repo) onto the qualified persisted grant key the
// v12 migration and the reconciler converge on. An ambiguous bare spelling
// is refused — it names neither same-name repository. An unregistered or
// syntactically unresolvable spelling is returned verbatim: grants for
// unregistered repositories are stored and listed as spelled, and stay
// inert until the repository registers (#245 BLOCKER B, #256).
func (service *API) canonicalAccessRepo(ctx context.Context, repo string) (string, error) {
	if service.repositories == nil {
		return repo, nil
	}
	resolved, err := service.repositories.RepositoryByName(ctx, repo)
	if err != nil {
		if errors.Is(err, store.ErrAmbiguous) {
			return "", fmt.Errorf("%w: %w", ErrInvalidInput, err)
		}
		// Unregistered or unparseable spellings keep their verbatim form.
		return repo, nil
	}
	qualified, err := service.secretAccess.QualifiedRepoName(ctx, resolved.ID)
	if err != nil {
		return repo, nil
	}
	return qualified, nil
}

func (service *API) accessList(ctx context.Context, repo string, includeRevoked bool) (api.AccessListResponse, error) {
	if service.secretAccess == nil {
		return api.AccessListResponse{}, fmt.Errorf("%w: secret access", ErrUnavailable)
	}
	// A non-empty filter accepts every repository spelling; persisted rows
	// key on the qualified form, so a bare filter passed through verbatim
	// would string-match nothing and misreport an empty grant set (#256).
	if strings.TrimSpace(repo) != "" {
		canonical, err := service.canonicalAccessRepo(ctx, repo)
		if err != nil {
			return api.AccessListResponse{}, err
		}
		repo = canonical
	}
	grants, err := service.secretAccess.SecretAccessList(ctx, repo, includeRevoked)
	if err != nil {
		return api.AccessListResponse{}, err
	}
	response := api.AccessListResponse{Grants: make([]api.AccessGrantResponse, len(grants))}
	for index, grant := range grants {
		response.Grants[index] = wireAccessGrant(grant)
	}
	return response, nil
}

func (service *API) accessAllow(ctx context.Context, actor api.Actor, repo, step, secret string) (api.AccessGrantResponse, error) {
	if !actor.Admin {
		return api.AccessGrantResponse{}, fmt.Errorf("%w: access_allow requires an admin uplink; mint one with: oberth uplink add --admin - <identity>@<host> < key.pub", ErrForbidden)
	}
	if service.secretAccess == nil {
		return api.AccessGrantResponse{}, fmt.Errorf("%w: secret access", ErrUnavailable)
	}
	if strings.TrimSpace(repo) == "" || strings.TrimSpace(step) == "" || strings.TrimSpace(secret) == "" {
		return api.AccessGrantResponse{}, fmt.Errorf("%w: repo, step, and secret are required", ErrInvalidInput)
	}
	// Resolve the repo name to its qualified form so same-name repos under
	// different upstreams cannot alias each other's grants (#245 BLOCKER B).
	canonical, err := service.canonicalAccessRepo(ctx, repo)
	if err != nil {
		return api.AccessGrantResponse{}, err
	}
	repo = canonical
	// Validate the entry before touching the ConfigMap. Without this check a
	// malformed entry (glob characters in repo/secret, or a non-wildcard step)
	// would be written to the ConfigMap, and the next reconciliation would
	// reject the entire list and revoke every active approval — a single
	// malformed request causing global secret revocation.
	if err := ValidateGrantEntry(0, SecretAccessGrantEntry{Repo: repo, Step: step, Secret: secret}); err != nil {
		return api.AccessGrantResponse{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if service.secretAccessReconciler != nil {
		if err := service.secretAccessReconciler.UpdateConfigMap(ctx, actor.Identity, AddGrant(repo, step, secret)); err != nil {
			return api.AccessGrantResponse{}, fmt.Errorf("update ConfigMap: %w", err)
		}
		// Read the grant back from sqlite after reconcile.
		grants, err := service.secretAccess.SecretAccessList(ctx, repo, false)
		if err != nil {
			return api.AccessGrantResponse{}, err
		}
		for _, grant := range grants {
			if grant.Repo == repo && grant.Step == step && grant.Secret == secret {
				return wireAccessGrant(grant), nil
			}
		}
		return api.AccessGrantResponse{}, fmt.Errorf("grant was written to ConfigMap but not found in store after reconcile")
	}
	grant, err := service.secretAccess.Grant(ctx, repo, step, secret, actor.Identity)
	if err != nil {
		return api.AccessGrantResponse{}, err
	}
	return wireAccessGrant(grant), nil
}

func (service *API) accessRevoke(ctx context.Context, actor api.Actor, repo, step, secret string) (api.AccessGrantResponse, error) {
	if !actor.Admin {
		return api.AccessGrantResponse{}, fmt.Errorf("%w: access_revoke requires an admin uplink; mint one with: oberth uplink add --admin - <identity>@<host> < key.pub", ErrForbidden)
	}
	if service.secretAccess == nil {
		return api.AccessGrantResponse{}, fmt.Errorf("%w: secret access", ErrUnavailable)
	}
	if strings.TrimSpace(repo) == "" || strings.TrimSpace(step) == "" || strings.TrimSpace(secret) == "" {
		return api.AccessGrantResponse{}, fmt.Errorf("%w: repo, step, and secret are required", ErrInvalidInput)
	}
	// Resolve the repo name to its qualified form for identity-safe
	// lookup (#245 BLOCKER B).
	canonical, err := service.canonicalAccessRepo(ctx, repo)
	if err != nil {
		return api.AccessGrantResponse{}, err
	}
	repo = canonical
	if service.secretAccessReconciler != nil {
		// Read the grant before removal so we can return it.
		grants, err := service.secretAccess.SecretAccessList(ctx, repo, false)
		if err != nil {
			return api.AccessGrantResponse{}, err
		}
		var target *store.SecretAccessGrant
		for i := range grants {
			if grants[i].Repo == repo && grants[i].Step == step && grants[i].Secret == secret {
				target = &grants[i]
				break
			}
		}
		if target == nil {
			return api.AccessGrantResponse{}, fmt.Errorf("%w: no active grant for %s/%s/%s", ErrInvalidInput, repo, step, secret)
		}
		if err := service.secretAccessReconciler.UpdateConfigMap(ctx, actor.Identity, RemoveGrant(repo, step, secret)); err != nil {
			return api.AccessGrantResponse{}, fmt.Errorf("update ConfigMap: %w", err)
		}
		// Read the revoked grant back.
		revoked, err := service.secretAccess.SecretAccessList(ctx, repo, true)
		if err != nil {
			return api.AccessGrantResponse{}, err
		}
		for _, grant := range revoked {
			if grant.ID == target.ID && grant.RevokedAt != nil {
				response := wireAccessGrant(grant)
				response.Warning = revokePolicySyncAdvisory
				return response, nil
			}
		}
		// Return the last known state with a synthetic revocation indicator.
		target.RevokedBy = "configmap"
		now := time.Now().UTC()
		target.RevokedAt = &now
		response := wireAccessGrant(*target)
		response.Warning = revokePolicySyncAdvisory
		return response, nil
	}
	grant, err := service.secretAccess.Revoke(ctx, repo, step, secret, actor.Identity)
	if err != nil {
		return api.AccessGrantResponse{}, err
	}
	response := wireAccessGrant(grant)
	response.Warning = revokePolicySyncAdvisory
	return response, nil
}

func (service *API) repoRemove(ctx context.Context, actor api.Actor, repo string) (any, error) {
	if !actor.Admin {
		return nil, fmt.Errorf("%w: repo_remove requires an admin uplink; mint one with: oberth uplink add --admin - <identity>@<host> < key.pub", ErrForbidden)
	}
	if service.repositoryRemover == nil {
		return nil, fmt.Errorf("%w: repository removal", ErrUnavailable)
	}
	if strings.TrimSpace(repo) == "" {
		return nil, fmt.Errorf("%w: repo is required", ErrInvalidInput)
	}
	removed, err := service.repositoryRemover.RemoveRepository(ctx, actor.Identity, repo)
	if err != nil {
		return nil, err
	}
	response := map[string]string{"removed": removed.Name, "upstream": removed.UpstreamName}
	// Best-effort git cache cleanup: the mapping is already gone, a stale
	// directory is inert, and the next push's Ensure recreates it. A failure
	// is still surfaced to the (admin) caller rather than swallowed, so an
	// operator knows a manual sweep is needed instead of discovering a stale
	// directory later. The cache path derives from the store's registered
	// name, never from raw tool input.
	if service.removeGitCache != nil {
		if cacheErr := service.removeGitCache(removed.Name); cacheErr != nil {
			response["cache_warning"] = cacheErr.Error()
		}
	}
	return response, nil
}

func wireAccessGrant(grant store.SecretAccessGrant) api.AccessGrantResponse {
	response := api.AccessGrantResponse{
		ID:         grant.ID,
		Repo:       grant.Repo,
		Step:       grant.Step,
		Secret:     grant.Secret,
		ApprovedBy: grant.ApprovedBy,
		ApprovedAt: grant.ApprovedAt.UTC().Format(time.RFC3339),
		RevokedBy:  grant.RevokedBy,
	}
	if grant.RevokedAt != nil {
		formatted := grant.RevokedAt.UTC().Format(time.RFC3339)
		response.RevokedAt = &formatted
	}
	return response
}

func decodeTool(raw json.RawMessage, value any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if len(raw) > maximumToolBytes {
		return fmt.Errorf("%w: tool arguments exceed 1 MiB", ErrInvalidInput)
	}
	decoder := json.NewDecoder(io.LimitReader(bytes.NewReader(raw), maximumToolBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("%w: decode tool arguments: %w", ErrInvalidInput, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: tool arguments must contain one JSON object", ErrInvalidInput)
	}
	return nil
}
