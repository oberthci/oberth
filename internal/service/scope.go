package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// Upstream-scoped uplinks (#073). A scoped actor's call runs on a copy of the
// API whose repository, run, issue, promotion, grant and secret-access
// dependencies are wrapped: every lookup outside the scope answers exactly
// like a missing record, and every list and count covers only in-scope
// repositories. Wrapping the dependencies, rather than checking each tool,
// keeps a new code path scoped by construction; the wrappers implement the
// full interfaces, so a method added to one fails to compile until it is
// scoped here.

// ScopeSource is the RunResolver capability scoped actors need: every
// repository's upstream and the upstream records. Without it a scoped call
// fails closed.
type ScopeSource interface {
	ListUpstreams(context.Context) ([]model.Upstream, error)
	ListRepositories(context.Context) ([]model.Repository, error)
}

// ScopedHealth is the optional Health capability that reports only what a
// scoped actor may see. Without it system_status fails closed for scoped
// actors.
type ScopedHealth interface {
	ScopedStatus(context.Context, api.Actor) (any, error)
}

type repoScope struct {
	actor     api.Actor
	upstreams map[int64]model.Upstream // in-scope upstreams by ID
	repos     map[int64]model.Repository
	repoIDs   []int64 // sorted, for SQL restrictions; never nil
	upIDs     []int64 // sorted, never nil
}

func (scope *repoScope) seesRepo(id int64) bool { _, ok := scope.repos[id]; return ok }

func (scope *repoScope) seesUpstreamID(id int64) bool { _, ok := scope.upstreams[id]; return ok }

// seesIssue: a CI issue by its repository, a manual issue by the upstream it
// is filed under; a workspace-global manual issue is never in scope.
func (scope *repoScope) seesIssue(issue model.Issue) bool {
	if issue.Kind == model.IssueCI || issue.RepoID != 0 {
		return scope.seesRepo(issue.RepoID)
	}
	return issue.ScopeUpstreamID != 0 && scope.seesUpstreamID(issue.ScopeUpstreamID)
}

// seesAccessKey: secret-access rows key on the qualified "upstream/org/repo";
// any other spelling (an unregistered bare name) is out of scope.
func (scope *repoScope) seesAccessKey(repo string) bool {
	parts := strings.Split(repo, "/")
	return len(parts) == 3 && scope.actor.SeesUpstream(parts[0])
}

func (scope *repoScope) qualified(repository model.Repository) string {
	return scope.upstreams[repository.UpstreamID].QualifiedRepo(repository.Name)
}

// forActor returns the API a call by actor runs on: itself for an unscoped
// actor, otherwise a copy whose dependencies see only the actor's scope.
// Calling it again on a scoped copy returns that copy.
func (service *API) forActor(ctx context.Context, actor api.Actor) (*API, error) {
	if service.scope != nil || !actor.Scoped() {
		return service, nil
	}
	source, ok := service.runs.(ScopeSource)
	if !ok {
		return nil, fmt.Errorf("%w: uplink scope", ErrUnavailable)
	}
	upstreams, err := source.ListUpstreams(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve uplink scope: %w", err)
	}
	repositories, err := source.ListRepositories(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve uplink scope: %w", err)
	}
	scope := &repoScope{actor: actor, upstreams: map[int64]model.Upstream{}, repos: map[int64]model.Repository{},
		repoIDs: []int64{}, upIDs: []int64{}}
	for _, upstream := range upstreams {
		if actor.SeesUpstream(upstream.Name) {
			scope.upstreams[upstream.ID] = upstream
			scope.upIDs = append(scope.upIDs, upstream.ID)
		}
	}
	for _, repository := range repositories {
		if scope.seesUpstreamID(repository.UpstreamID) {
			scope.repos[repository.ID] = repository
			scope.repoIDs = append(scope.repoIDs, repository.ID)
		}
	}
	sort.Slice(scope.repoIDs, func(i, j int) bool { return scope.repoIDs[i] < scope.repoIDs[j] })
	sort.Slice(scope.upIDs, func(i, j int) bool { return scope.upIDs[i] < scope.upIDs[j] })

	scoped := *service
	scoped.scope = scope
	scoped.runs = scopedRuns{inner: service.runs, scope: scope}
	if service.history != nil {
		scoped.history = scopedHistory{inner: service.history, scope: scope}
	}
	if service.repositories != nil {
		scoped.repositories = scopedRepositories{inner: service.repositories, scope: scope}
	}
	if service.issues != nil {
		scoped.issues = scopedIssues{inner: service.issues, scope: scope}
	}
	if service.promotions != nil {
		scoped.promotions = scopedPromotions{PromotionRepository: service.promotions, scope: scope}
	}
	if service.secretAccess != nil {
		scoped.secretAccess = scopedSecretAccess{inner: service.secretAccess, scope: scope}
	}
	if service.promotionGrants != nil {
		scoped.promotionGrants = scopedPromotionGrants{inner: service.promotionGrants, scope: scope}
	}
	if service.repositoryRemover != nil {
		scoped.repositoryRemover = scopedRemover{inner: service.repositoryRemover, scope: scope}
	}
	// The secret-store tools act on the whole installation; CallTool refuses
	// them to a scoped actor, and this copy could not reach them anyway.
	scoped.secretStoreVerifier = nil
	return &scoped, nil
}

// requireUnscoped refuses installation-wide operations to a scoped actor.
func requireUnscoped(actor api.Actor, operation string) error {
	if actor.Scoped() {
		return fmt.Errorf("%w: %s acts on the whole installation and is not available to a scoped uplink", ErrForbidden, operation)
	}
	return nil
}

func hiddenRepository(name string) error {
	return fmt.Errorf("%w: repository %q", store.ErrNotFound, name)
}

func hiddenRun() error { return fmt.Errorf("%w: run", store.ErrNotFound) }

func hiddenIssue() error { return fmt.Errorf("%w: issue", store.ErrNotFound) }

// lookupRepository resolves a repository spelling among in-scope
// repositories only, so neither a hit nor an ambiguity error can name an
// out-of-scope repository or upstream.
func (scope *repoScope) lookupRepository(name string) (model.Repository, error) {
	spelled := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(name), "/"), ".git")
	parts := strings.Split(spelled, "/")
	if spelled == "" || len(parts) > 3 {
		return model.Repository{}, fmt.Errorf("%w: repository selector %q", store.ErrInvalid, name)
	}
	var matches []model.Repository
	for _, repository := range scope.repos {
		upstream := scope.upstreams[repository.UpstreamID]
		org := upstream.Org()
		if org == "" {
			org = upstream.Name
		}
		switch len(parts) {
		case 1:
			if repository.Name != parts[0] {
				continue
			}
		case 2:
			if org != parts[0] || repository.Name != parts[1] {
				continue
			}
		case 3:
			if upstream.Name != parts[0] || org != parts[1] || repository.Name != parts[2] {
				continue
			}
		}
		matches = append(matches, repository)
	}
	switch len(matches) {
	case 0:
		return model.Repository{}, hiddenRepository(name)
	case 1:
		return matches[0], nil
	default:
		names := make([]string, 0, len(matches))
		for _, match := range matches {
			names = append(names, scope.qualified(match))
		}
		sort.Strings(names)
		return model.Repository{}, fmt.Errorf("%w: repository %q matches %s; use upstream/org/repo",
			store.ErrAmbiguous, name, strings.Join(names, ", "))
	}
}

func (scope *repoScope) visibleRepositories(repositories []model.Repository) []model.Repository {
	visible := make([]model.Repository, 0, len(repositories))
	for _, repository := range repositories {
		if scope.seesRepo(repository.ID) {
			visible = append(visible, repository)
		}
	}
	return visible
}

func (scope *repoScope) visibleRuns(runs []model.Run) []model.Run {
	visible := make([]model.Run, 0, len(runs))
	for _, run := range runs {
		if scope.seesRepo(run.RepoID) {
			visible = append(visible, run)
		}
	}
	return visible
}

// resolveScopedRepository keeps the store's resolution when it lands in
// scope and otherwise resolves among in-scope repositories, so its errors
// never describe what lies outside.
func resolveScopedRepository(ctx context.Context, scope *repoScope, lookup func(context.Context, string) (model.Repository, error), name string) (model.Repository, error) {
	repository, err := lookup(ctx, name)
	if err == nil && scope.seesRepo(repository.ID) {
		return repository, nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrAmbiguous) && !errors.Is(err, store.ErrInvalid) {
		return model.Repository{}, err
	}
	return scope.lookupRepository(name)
}

// scopedRuns is the RunResolver for a scoped actor, including the optional
// capabilities the API probes for.
type scopedRuns struct {
	inner RunResolver
	scope *repoScope
}

func (runs scopedRuns) RepositoryByName(ctx context.Context, name string) (model.Repository, error) {
	return resolveScopedRepository(ctx, runs.scope, runs.inner.RepositoryByName, name)
}

func (runs scopedRuns) ListRepositories(ctx context.Context) ([]model.Repository, error) {
	repositories, err := runs.inner.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	return runs.scope.visibleRepositories(repositories), nil
}

func (runs scopedRuns) ResolveRun(ctx context.Context, repoID int64, selector string) (model.Run, error) {
	if !runs.scope.seesRepo(repoID) {
		return model.Run{}, fmt.Errorf("%w: run selector %q", store.ErrNotFound, selector)
	}
	return runs.inner.ResolveRun(ctx, repoID, selector)
}

func (runs scopedRuns) ResolveWaitRun(ctx context.Context, repoID int64, selector, trigger string) (model.Run, error) {
	if !runs.scope.seesRepo(repoID) {
		return model.Run{}, fmt.Errorf("%w: run selector %q", store.ErrNotFound, selector)
	}
	return runs.inner.ResolveWaitRun(ctx, repoID, selector, trigger)
}

// Upstream forwards upstreamResolver for in-scope upstreams.
func (runs scopedRuns) Upstream(ctx context.Context, id int64) (model.Upstream, error) {
	resolver, ok := runs.inner.(upstreamResolver)
	if !ok {
		return model.Upstream{}, fmt.Errorf("%w: upstream lookup", ErrUnavailable)
	}
	if !runs.scope.seesUpstreamID(id) {
		return model.Upstream{}, fmt.Errorf("%w: upstream", store.ErrNotFound)
	}
	return resolver.Upstream(ctx, id)
}

// DistinctBranchRefsForSHA forwards BranchRefResolver; without it the inner
// resolver reports no ambiguity, as the API assumes when the capability is
// absent.
func (runs scopedRuns) DistinctBranchRefsForSHA(ctx context.Context, repoID int64, sha string) ([]string, error) {
	resolver, ok := runs.inner.(BranchRefResolver)
	if !ok || !runs.scope.seesRepo(repoID) {
		return nil, nil
	}
	return resolver.DistinctBranchRefsForSHA(ctx, repoID, sha)
}

// QueuedRunPosition withholds the queue position: it counts every queued
// run ahead, in or out of scope, so it would reveal out-of-scope volume.
func (runs scopedRuns) QueuedRunPosition(context.Context, string) (int64, error) {
	return 0, fmt.Errorf("%w: queue position is not shown to a scoped uplink", ErrUnavailable)
}

type scopedHistory struct {
	inner RunHistory
	scope *repoScope
}

// runIDResolver is the RunHistory capability that resolves an ID prefix among
// some repositories only, so a prefix shared with an out-of-scope run is
// neither ambiguous nor a hint that such a run exists.
type runIDResolver interface {
	ResolveRunIDIn(context.Context, string, []int64) (model.Run, error)
}

func (history scopedHistory) ResolveRunID(ctx context.Context, id string) (model.Run, error) {
	if resolver, ok := history.inner.(runIDResolver); ok {
		return resolver.ResolveRunIDIn(ctx, id, history.scope.repoIDs)
	}
	run, err := history.inner.ResolveRunID(ctx, id)
	if err != nil {
		return model.Run{}, err
	}
	if !history.scope.seesRepo(run.RepoID) {
		if trimmed := strings.ToLower(strings.TrimSpace(id)); len(trimmed) < 32 {
			return model.Run{}, fmt.Errorf("%w: run ID %q", store.ErrNotFound, trimmed)
		}
		return model.Run{}, hiddenRun()
	}
	return run, nil
}

func (history scopedHistory) ListRecentRuns(ctx context.Context, filter model.RunListFilter) ([]model.Run, error) {
	if filter.RepoID != 0 && !history.scope.seesRepo(filter.RepoID) {
		return []model.Run{}, nil
	}
	filter.RepoIDs = history.scope.repoIDs
	return history.inner.ListRecentRuns(ctx, filter)
}

func (history scopedHistory) ListLatestRunsPerRepo(ctx context.Context, perRepo int) ([]model.Run, error) {
	runs, err := history.inner.ListLatestRunsPerRepo(ctx, perRepo)
	if err != nil {
		return nil, err
	}
	return history.scope.visibleRuns(runs), nil
}

func (history scopedHistory) CountRuns(ctx context.Context) (model.RunCounts, error) {
	counts, err := history.inner.CountRuns(ctx)
	if err != nil {
		return model.RunCounts{}, err
	}
	scoped := model.RunCounts{ByRepo: map[int64]int{}}
	for repoID, count := range counts.ByRepo {
		if history.scope.seesRepo(repoID) {
			scoped.ByRepo[repoID] = count
			scoped.Total += count
		}
	}
	return scoped, nil
}

func (history scopedHistory) Run(ctx context.Context, id string) (model.Run, error) {
	run, err := history.inner.Run(ctx, id)
	if err != nil {
		return model.Run{}, err
	}
	if !history.scope.seesRepo(run.RepoID) {
		return model.Run{}, hiddenRun()
	}
	return run, nil
}

func (history scopedHistory) StepResults(ctx context.Context, runID string) ([]model.StepResult, error) {
	if _, err := history.Run(ctx, runID); err != nil {
		return nil, err
	}
	return history.inner.StepResults(ctx, runID)
}

type scopedRepositories struct {
	inner RepositoryReader
	scope *repoScope
}

func (repositories scopedRepositories) Repository(ctx context.Context, id int64) (model.Repository, error) {
	if !repositories.scope.seesRepo(id) {
		return model.Repository{}, fmt.Errorf("%w: repository", store.ErrNotFound)
	}
	return repositories.inner.Repository(ctx, id)
}

func (repositories scopedRepositories) RepositoryByName(ctx context.Context, name string) (model.Repository, error) {
	return resolveScopedRepository(ctx, repositories.scope, repositories.inner.RepositoryByName, name)
}

func (repositories scopedRepositories) ListRepositories(ctx context.Context) ([]model.Repository, error) {
	list, err := repositories.inner.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	return repositories.scope.visibleRepositories(list), nil
}

type scopedIssues struct {
	inner IssueRepository
	scope *repoScope
}

// require answers like a missing issue unless the issue is in scope.
func (issues scopedIssues) require(ctx context.Context, id int64) error {
	issue, err := issues.inner.Issue(ctx, id)
	if err != nil {
		return err
	}
	if !issues.scope.seesIssue(issue) {
		return hiddenIssue()
	}
	return nil
}

func (issues scopedIssues) CreateManualIssue(ctx context.Context, actor string, spec model.ManualIssueSpec) (model.Issue, error) {
	if spec.ScopeUpstreamID == 0 || !issues.scope.seesUpstreamID(spec.ScopeUpstreamID) {
		return model.Issue{}, fmt.Errorf("%w: a scoped uplink files issues under one of its upstreams", ErrInvalidInput)
	}
	return issues.inner.CreateManualIssue(ctx, actor, spec)
}

func (issues scopedIssues) Issue(ctx context.Context, id int64) (model.Issue, error) {
	issue, err := issues.inner.Issue(ctx, id)
	if err != nil {
		return model.Issue{}, err
	}
	if !issues.scope.seesIssue(issue) {
		return model.Issue{}, hiddenIssue()
	}
	return issue, nil
}

func (issues scopedIssues) UpdateIssue(ctx context.Context, actor string, id int64, patch model.IssuePatch) (model.Issue, error) {
	if err := issues.require(ctx, id); err != nil {
		return model.Issue{}, err
	}
	return issues.inner.UpdateIssue(ctx, actor, id, patch)
}

func (issues scopedIssues) UpdateManualIssue(ctx context.Context, actor string, id int64, patch model.IssuePatch) (model.Issue, error) {
	if err := issues.require(ctx, id); err != nil {
		return model.Issue{}, err
	}
	return issues.inner.UpdateManualIssue(ctx, actor, id, patch)
}

func (issues scopedIssues) AppendIssueBody(ctx context.Context, actor string, id int64, text string) (model.Issue, error) {
	if err := issues.require(ctx, id); err != nil {
		return model.Issue{}, err
	}
	return issues.inner.AppendIssueBody(ctx, actor, id, text)
}

func (issues scopedIssues) ReopenManualIssue(ctx context.Context, actor string, id int64) (model.Issue, error) {
	if err := issues.require(ctx, id); err != nil {
		return model.Issue{}, err
	}
	return issues.inner.ReopenManualIssue(ctx, actor, id)
}

func (issues scopedIssues) DeleteManualIssue(ctx context.Context, actor string, id int64) error {
	if err := issues.require(ctx, id); err != nil {
		return err
	}
	return issues.inner.DeleteManualIssue(ctx, actor, id)
}

func (issues scopedIssues) ListIssues(ctx context.Context, filter model.IssueListFilter) (model.IssuePage, error) {
	if filter.RepoID != 0 && !issues.scope.seesRepo(filter.RepoID) {
		return model.IssuePage{}, nil
	}
	filter.UpstreamIDs = issues.scope.upIDs
	return issues.inner.ListIssues(ctx, filter)
}

func (issues scopedIssues) AcquireIssueLock(ctx context.Context, id int64, owner string) (model.IssueLock, error) {
	if err := issues.require(ctx, id); err != nil {
		return model.IssueLock{}, err
	}
	return issues.inner.AcquireIssueLock(ctx, id, owner)
}

func (issues scopedIssues) RenewIssueLock(ctx context.Context, id int64, owner string) (model.IssueLock, error) {
	if err := issues.require(ctx, id); err != nil {
		return model.IssueLock{}, err
	}
	return issues.inner.RenewIssueLock(ctx, id, owner)
}

func (issues scopedIssues) OpenCIIssue(ctx context.Context, repoID int64, branch string) (model.Issue, error) {
	if !issues.scope.seesRepo(repoID) {
		return model.Issue{}, fmt.Errorf("%w: open CI issue", store.ErrNotFound)
	}
	return issues.inner.OpenCIIssue(ctx, repoID, branch)
}

func (issues scopedIssues) CloseCIIssue(ctx context.Context, actor string, id int64, resolution string) (model.Issue, error) {
	if err := issues.require(ctx, id); err != nil {
		return model.Issue{}, err
	}
	return issues.inner.CloseCIIssue(ctx, actor, id, resolution)
}

// scopedPromotions scopes the promotion reads a tool can reach. Publication
// and workspace-cleanup methods pass through: they act on records the scoped
// call already resolved through Promotion or a scoped run.
type scopedPromotions struct {
	PromotionRepository
	scope *repoScope
}

func (promotions scopedPromotions) ListPromotions(ctx context.Context, filter model.PromotionListFilter) (model.PromotionPage, error) {
	if filter.RepoID != 0 && !promotions.scope.seesRepo(filter.RepoID) {
		return model.PromotionPage{Promotions: []model.Promotion{}}, nil
	}
	filter.RepoIDs = promotions.scope.repoIDs
	return promotions.PromotionRepository.ListPromotions(ctx, filter)
}

func (promotions scopedPromotions) Promotion(ctx context.Context, id string) (model.Promotion, error) {
	promotion, err := promotions.PromotionRepository.Promotion(ctx, id)
	if err != nil {
		return model.Promotion{}, err
	}
	if !promotions.scope.seesRepo(promotion.RepoID) {
		return model.Promotion{}, fmt.Errorf("%w: promotion", store.ErrNotFound)
	}
	return promotion, nil
}

func (promotions scopedPromotions) PromotionByRun(ctx context.Context, runID string) (model.Promotion, error) {
	promotion, err := promotions.PromotionRepository.PromotionByRun(ctx, runID)
	if err != nil {
		return model.Promotion{}, err
	}
	if !promotions.scope.seesRepo(promotion.RepoID) {
		return model.Promotion{}, fmt.Errorf("%w: promotion", store.ErrNotFound)
	}
	return promotion, nil
}

func (promotions scopedPromotions) AppendPromotion(ctx context.Context, spec model.PromotionSpec) (model.Promotion, error) {
	if !promotions.scope.seesRepo(spec.RepoID) {
		return model.Promotion{}, hiddenRepository(fmt.Sprint(spec.RepoID))
	}
	return promotions.PromotionRepository.AppendPromotion(ctx, spec)
}

type scopedSecretAccess struct {
	inner SecretAccessStore
	scope *repoScope
}

func (access scopedSecretAccess) SecretAccessList(ctx context.Context, repo string, includeRevoked bool) ([]store.SecretAccessGrant, error) {
	if repo != "" && !access.scope.seesAccessKey(repo) {
		return []store.SecretAccessGrant{}, nil
	}
	grants, err := access.inner.SecretAccessList(ctx, repo, includeRevoked)
	if err != nil {
		return nil, err
	}
	visible := make([]store.SecretAccessGrant, 0, len(grants))
	for _, grant := range grants {
		if access.scope.seesAccessKey(grant.Repo) {
			visible = append(visible, grant)
		}
	}
	return visible, nil
}

func (access scopedSecretAccess) Grant(ctx context.Context, repo, step, secret, actor string) (store.SecretAccessGrant, error) {
	if !access.scope.seesAccessKey(repo) {
		return store.SecretAccessGrant{}, hiddenRepository(repo)
	}
	return access.inner.Grant(ctx, repo, step, secret, actor)
}

func (access scopedSecretAccess) Revoke(ctx context.Context, repo, step, secret, actor string) (store.SecretAccessGrant, error) {
	if !access.scope.seesAccessKey(repo) {
		return store.SecretAccessGrant{}, hiddenRepository(repo)
	}
	return access.inner.Revoke(ctx, repo, step, secret, actor)
}

func (access scopedSecretAccess) QualifiedRepoName(ctx context.Context, repoID int64) (string, error) {
	if !access.scope.seesRepo(repoID) {
		return "", fmt.Errorf("%w: repository", store.ErrNotFound)
	}
	return access.inner.QualifiedRepoName(ctx, repoID)
}

func (access scopedSecretAccess) RepositoryByName(ctx context.Context, name string) (model.Repository, error) {
	return resolveScopedRepository(ctx, access.scope, access.inner.RepositoryByName, name)
}

func (access scopedSecretAccess) PendingGrantDeclarations(ctx context.Context, repo string) ([]store.PendingGrantDeclaration, error) {
	if repo != "" && !access.scope.seesAccessKey(repo) {
		return []store.PendingGrantDeclaration{}, nil
	}
	pending, err := access.inner.PendingGrantDeclarations(ctx, repo)
	if err != nil {
		return nil, err
	}
	visible := make([]store.PendingGrantDeclaration, 0, len(pending))
	for _, declaration := range pending {
		if access.scope.seesAccessKey(declaration.Repo) {
			visible = append(visible, declaration)
		}
	}
	return visible, nil
}

type scopedPromotionGrants struct {
	inner PromotionGrantStore
	scope *repoScope
}

func (grants scopedPromotionGrants) AppendPromotionGrant(ctx context.Context, spec model.PromotionGrantSpec) (model.PromotionGrant, error) {
	if !grants.scope.seesRepo(spec.RepoID) {
		return model.PromotionGrant{}, fmt.Errorf("%w: repository", store.ErrNotFound)
	}
	return grants.inner.AppendPromotionGrant(ctx, spec)
}

func (grants scopedPromotionGrants) RevokePromotionGrant(ctx context.Context, id, actor string) (model.PromotionGrant, error) {
	if _, err := grants.PromotionGrant(ctx, id); err != nil {
		return model.PromotionGrant{}, err
	}
	return grants.inner.RevokePromotionGrant(ctx, id, actor)
}

func (grants scopedPromotionGrants) MatchingPromotionGrant(ctx context.Context, repoID int64, sha, target string) (model.PromotionGrant, error) {
	if !grants.scope.seesRepo(repoID) {
		return model.PromotionGrant{}, fmt.Errorf("%w: promotion grant", store.ErrNotFound)
	}
	return grants.inner.MatchingPromotionGrant(ctx, repoID, sha, target)
}

func (grants scopedPromotionGrants) ConsumePromotionGrant(ctx context.Context, grantID, promotionID, actor string) error {
	if _, err := grants.PromotionGrant(ctx, grantID); err != nil {
		return err
	}
	return grants.inner.ConsumePromotionGrant(ctx, grantID, promotionID, actor)
}

func (grants scopedPromotionGrants) ListPromotionGrants(ctx context.Context, filter model.PromotionGrantListFilter) ([]model.PromotionGrant, error) {
	if filter.RepoID != 0 && !grants.scope.seesRepo(filter.RepoID) {
		return []model.PromotionGrant{}, nil
	}
	list, err := grants.inner.ListPromotionGrants(ctx, filter)
	if err != nil {
		return nil, err
	}
	visible := make([]model.PromotionGrant, 0, len(list))
	for _, grant := range list {
		if grants.scope.seesRepo(grant.RepoID) {
			visible = append(visible, grant)
		}
	}
	return visible, nil
}

func (grants scopedPromotionGrants) PromotionGrant(ctx context.Context, id string) (model.PromotionGrant, error) {
	grant, err := grants.inner.PromotionGrant(ctx, id)
	if err != nil {
		return model.PromotionGrant{}, err
	}
	if !grants.scope.seesRepo(grant.RepoID) {
		return model.PromotionGrant{}, fmt.Errorf("%w: promotion grant", store.ErrNotFound)
	}
	return grant, nil
}

type scopedRemover struct {
	inner RepositoryRemover
	scope *repoScope
}

func (remover scopedRemover) RemoveRepository(ctx context.Context, actor, name string) (store.RemovedRepository, error) {
	repository, err := remover.scope.lookupRepository(name)
	if err != nil {
		return store.RemovedRepository{}, err
	}
	// Remove exactly the in-scope repository, by its qualified name.
	return remover.inner.RemoveRepository(ctx, actor, remover.scope.qualified(repository))
}
