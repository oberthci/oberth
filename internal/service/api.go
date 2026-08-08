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
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

const promotionCompensationTimeout = 30 * time.Second

type APIConfig struct {
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
	Auditor                Auditor
	Health                 Health
	Signals                *Signals
	MaximumWait            time.Duration
	MutationGate           func(context.Context) error
	PromotionWorkspaceRoot string
	RemoveWorkspace        func(string) error
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
	refs                   RefResolver
	auditor                Auditor
	health                 Health
	signals                *Signals
	maximumWait            time.Duration
	mutationGate           func(context.Context) error
	promotionWorkspaceRoot string
	workspaces             *workspaceLifecycle
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
		enqueues: config.Enqueues, git: config.Git, refs: config.Refs, logs: config.Logs, auditor: config.Auditor,
		health: config.Health, signals: signals, maximumWait: maximumWait, mutationGate: mutationGate,
		promotionWorkspaceRoot: promotionWorkspaceRoot, workspaces: workspaces,
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
			Repo string `json:"repo"`
			SHA  string `json:"sha"`
			Step string `json:"step"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.logsFor(ctx, arguments.Repo, arguments.SHA, arguments.Step)
	case "wait":
		var arguments struct {
			Repo    string `json:"repo"`
			SHA     string `json:"sha"`
			Timeout int    `json:"timeout"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.waitRun(ctx, arguments.Repo, arguments.SHA, arguments.Timeout, actor.Identity)
	case "sync":
		var arguments struct {
			Repo string `json:"repo"`
			SHA  string `json:"sha"`
		}
		if err := decodeTool(raw, &arguments); err != nil {
			return nil, err
		}
		return service.sync(ctx, actor, arguments.Repo, arguments.SHA)
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
	default:
		return nil, fmt.Errorf("%w: unknown tool %q", ErrInvalidInput, name)
	}
}

func (service *API) Runs(ctx context.Context, _ api.Actor, limit int) (any, error) {
	if service.history == nil {
		return nil, fmt.Errorf("%w: run history", ErrUnavailable)
	}
	return service.history.ListRecentRuns(ctx, model.RunListFilter{Limit: limit})
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
	steps, err := service.history.StepResults(ctx, run.ID)
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

// RunLog is the read-only dashboard view of one recorded step's bounded
// retained log. The burn and step must name an exactly recorded step result
// for the run, so the dashboard can never probe the log store outside the
// run's own indexed ranges.
func (service *API) RunLog(ctx context.Context, _ api.Actor, id, burn, step string) (any, error) {
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
	steps, err := service.history.StepResults(ctx, run.ID)
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
	body, err := service.logs.Read(run.ID, burn, step)
	if err != nil {
		return nil, fmt.Errorf("read bounded run log: %w", err)
	}
	return LogResponse{RunID: run.ID, Burn: burn, Step: step, Output: string(body)}, nil
}

func (service *API) Repositories(ctx context.Context, _ api.Actor) (any, error) {
	if service.repositories != nil {
		return service.repositories.ListRepositories(ctx)
	}
	return service.runs.ListRepositories(ctx)
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
			return service.statusRefWithoutRun(ctx, repositoryName, selector)
		}
		return StatusResponse{}, err
	}
	response := StatusResponse{
		Repository: repository, Run: run, Repo: repository.Name, Ref: run.Ref,
		SHA: run.SHA, RunID: run.ID, Status: wireRunStatus(run.Status), Burns: map[string]string{},
	}
	if service.history != nil {
		response.Steps, err = service.history.StepResults(ctx, run.ID)
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
			if actor != "" {
				if err := service.renewIssueLock(ctx, issue.ID, actor); err != nil {
					return StatusResponse{}, fmt.Errorf("renew run issue lock: %w", err)
				}
			}
		}
	}
	return response, nil
}

// statusRefWithoutRun resolves a selector as a branch name in the bare Git
// cache when no run exists. It mirrors resolveRun's repository disambiguation.
func (service *API) statusRefWithoutRun(ctx context.Context, repositoryName, selector string) (StatusResponse, error) {
	if strings.TrimSpace(repositoryName) != "" {
		repository, err := service.runs.RepositoryByName(ctx, repositoryName)
		if err != nil {
			return StatusResponse{}, err
		}
		sha, err := service.refs.RefSHA(ctx, repository.Name, selector)
		if err != nil {
			return StatusResponse{}, fmt.Errorf("%w: run selector %q", store.ErrNotFound, selector)
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
	return StatusResponse{
		Repository: matched, Repo: matched.Name, Ref: selector,
		SHA: matchedSHA, Status: "no-runs", Burns: map[string]string{},
	}, nil
}

func wireRunStatus(status model.RunStatus) string {
	if status == model.RunPassed {
		return "delivered"
	}
	return string(status)
}

func wireBurns(steps []model.StepResult, run model.Run) (map[string]string, *int) {
	burns := make(map[string]string)
	var exitCode *int
	for _, step := range steps {
		current := burns[step.Burn]
		switch step.Status {
		case model.StepFailed, model.StepTimedOut:
			burns[step.Burn] = "failed"
			if exitCode == nil && (run.FailedStep == "" || step.Step == run.FailedStep || step.Burn == run.FailedBurn) {
				value := step.ExitCode
				exitCode = &value
			}
		case model.StepPassed:
			if current != "failed" {
				burns[step.Burn] = "passed"
			}
		case model.StepSkipped:
			if current == "" {
				burns[step.Burn] = "-"
			}
		}
	}
	return burns, exitCode
}

func (service *API) logsFor(ctx context.Context, repositoryName, selector, wanted string) (LogResponse, error) {
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
	steps, err := service.history.StepResults(ctx, run.ID)
	if err != nil {
		return LogResponse{}, fmt.Errorf("load run steps: %w", err)
	}
	burn, step, err := selectLogRange(steps, wanted)
	if err != nil {
		return LogResponse{}, err
	}
	body, err := service.logs.Read(run.ID, burn, step)
	if err != nil {
		return LogResponse{}, fmt.Errorf("read bounded run log: %w", err)
	}
	return LogResponse{RunID: run.ID, Burn: burn, Step: step, Output: string(body)}, nil
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

func (service *API) waitRun(ctx context.Context, repositoryName, selector string, requestedSeconds int, actor string) (WaitResponse, error) {
	if !validSHASelector(selector) {
		return WaitResponse{}, fmt.Errorf("%w: full or short SHA is required", ErrInvalidInput)
	}
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
		if status.Run.Status.Terminal() {
			return WaitResponse{StatusResponse: status}, nil
		}
		changed := service.signals.Run(status.Run.ID)
		// Re-read after subscribing so a transition between the first read and
		// channel lookup cannot be lost.
		status, err = service.status(ctx, repositoryName, selector, actor)
		if err != nil {
			return WaitResponse{}, err
		}
		if status.Run.Status.Terminal() {
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

func (service *API) sync(ctx context.Context, actor api.Actor, repositoryName, selector string) (model.Run, error) {
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
	// A divergent promotion run becomes visible to the scheduler as soon as it
	// is enqueued. Record the required promotion audit before that admission so
	// no competing queue wake can claim unaudited work.
	if err := service.auditPromotion(ctx, actor.Identity, promotion); err != nil {
		return service.failAdmittedPromotion(ctx, promotion, err.Error(), "")
	}
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
			return PromotionWaitResponse{Promotion: promotion, StillRunning: true}, nil
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

func (service *API) issueGet(ctx context.Context, actor api.Actor, id int64) (model.Issue, error) {
	if service.issues == nil || id <= 0 {
		return model.Issue{}, fmt.Errorf("%w: issue ID is required", ErrInvalidInput)
	}
	issue, err := service.issues.Issue(ctx, id)
	if err != nil {
		return model.Issue{}, err
	}
	if err := service.renewIssueLock(ctx, issue.ID, actor.Identity); err != nil {
		return model.Issue{}, err
	}
	return issue, nil
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
	}
}

func wireIssueList(page model.IssuePage) api.IssueListResponse {
	issues := make([]api.IssueListItem, len(page.Issues))
	for index, issue := range page.Issues {
		issues[index] = api.IssueListItem{ID: issue.ID, State: string(issue.State)}
	}
	return api.IssueListResponse{Issues: issues, NextBefore: page.NextBefore}
}

func wireIssueLock(lock model.IssueLock) api.IssueLockResponse {
	return api.IssueLockResponse{
		ID: lock.IssueID, Owner: lock.Owner,
		ExpiresAt: lock.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}

func (service *API) renewIssueLock(ctx context.Context, issueID int64, actor string) error {
	if service.mutationGate != nil && service.mutationGate(ctx) != nil {
		return nil
	}
	_, err := service.issues.RenewIssueLock(ctx, issueID, actor)
	if errors.Is(err, store.ErrLockNotOwned) || errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

func mutatingTool(name string) bool {
	switch name {
	case "sync", "promote", "issue_create", "issue_update", "issue_close", "issue_delete", "issue_lock":
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

func (service *API) auditPromotion(ctx context.Context, actor string, promotion model.Promotion) error {
	_, err := service.auditor.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: actor, Action: "promotion." + string(promotion.Status), ResourceType: "promotion", ResourceID: promotion.ID,
		Details: fmt.Sprintf(`{"source":%q,"target":%q,"result":%q}`, promotion.SourceSHA, promotion.TargetRef, promotion.ResultSHA),
	})
	if err != nil {
		return fmt.Errorf("audit promotion: %w", err)
	}
	return nil
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
