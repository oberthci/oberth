package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
)

// Upstream branch deletion is asynchronous: a receive or prune deletes the
// mirror ref and records a pending deletion; this propagator later deletes
// the upstream refs, one push per repository batch, with bounded retries.

const (
	branchDeletionInterval    = time.Minute
	branchDeletionPassBudget  = 3 * time.Minute
	branchDeletionMaxAttempts = 6
	branchDeletionBatchLimit  = 2000
	branchDeletionActor       = "oberth:branch-deletion"
	branchDeletionAuditNames  = 50
)

type BranchDeletionStore interface {
	DueBranchDeletions(context.Context, time.Time, int) ([]model.BranchDeletion, error)
	UpdateBranchDeletions(context.Context, []model.BranchDeletionUpdate) error
	QualifiedRepoName(context.Context, int64) (string, error)
}

type BranchDeletionGit interface {
	PropagateBranchDeletions(context.Context, string, []gitcache.UpstreamDeletion) ([]gitcache.UpstreamDeletionResult, error)
	AbandonBranchDeletions(context.Context, string, []string) error
}

type BranchDeletionPropagator struct {
	store   BranchDeletionStore
	git     BranchDeletionGit
	auditor Auditor
	gate    func(context.Context) error
	log     *log.Logger
	wake    chan struct{}
	now     func() time.Time
	budget  time.Duration
}

func NewBranchDeletionPropagator(database BranchDeletionStore, git BranchDeletionGit, auditor Auditor, gate func(context.Context) error, logger *log.Logger) (*BranchDeletionPropagator, error) {
	if database == nil || git == nil || auditor == nil || gate == nil || logger == nil {
		return nil, errors.New("service: branch deletion propagator requires store, Git, auditor, mutation gate, and logger")
	}
	return &BranchDeletionPropagator{store: database, git: git, auditor: auditor, gate: gate, log: logger,
		wake: make(chan struct{}, 1), now: time.Now, budget: branchDeletionPassBudget}, nil
}

// Notify wakes the propagator after a deletion was recorded. It never blocks.
func (p *BranchDeletionPropagator) Notify() {
	if p == nil {
		return
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *BranchDeletionPropagator) Run(ctx context.Context) error {
	ticker := time.NewTicker(branchDeletionInterval)
	defer ticker.Stop()
	for {
		if _, err := p.Propagate(ctx); err != nil && ctx.Err() == nil {
			p.log.Printf("upstream branch deletion: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-p.wake:
		}
	}
}

// Propagate runs one pass over every due record and returns how many records
// reached a final state.
func (p *BranchDeletionPropagator) Propagate(ctx context.Context) (int, error) {
	if err := p.gate(ctx); err != nil {
		return 0, fmt.Errorf("audit chain is not writable: %w", err)
	}
	due, err := p.store.DueBranchDeletions(ctx, p.now(), branchDeletionBatchLimit)
	if err != nil {
		return 0, err
	}
	var order []int64
	byRepo := make(map[int64][]model.BranchDeletion)
	for _, deletion := range due {
		if _, seen := byRepo[deletion.RepoID]; !seen {
			order = append(order, deletion.RepoID)
		}
		byRepo[deletion.RepoID] = append(byRepo[deletion.RepoID], deletion)
	}
	finished := 0
	var errs []error
	for _, repoID := range order {
		if ctx.Err() != nil {
			break
		}
		count, err := p.propagateRepository(ctx, byRepo[repoID])
		finished += count
		if err != nil {
			errs = append(errs, err)
		}
	}
	return finished, errors.Join(errs...)
}

func (p *BranchDeletionPropagator) propagateRepository(ctx context.Context, deletions []model.BranchDeletion) (int, error) {
	repository := deletions[0].Repository
	input, err := p.store.QualifiedRepoName(ctx, deletions[0].RepoID)
	if err != nil {
		return 0, fmt.Errorf("resolve repository %s: %w", repository, err)
	}
	requests := make([]gitcache.UpstreamDeletion, len(deletions))
	for index, deletion := range deletions {
		requests[index] = gitcache.UpstreamDeletion{Branch: deletion.Branch, OldSHA: deletion.OldSHA}
	}
	passCtx, cancel := context.WithTimeout(ctx, p.budget)
	results, passErr := p.git.PropagateBranchDeletions(passCtx, input, requests)
	cancel()
	if passErr != nil {
		results = make([]gitcache.UpstreamDeletionResult, len(deletions))
		for index := range results {
			results[index] = gitcache.UpstreamDeletionResult{Branch: deletions[index].Branch, State: gitcache.UpstreamRetry, Err: passErr}
		}
	}
	if len(results) != len(deletions) {
		return 0, fmt.Errorf("repository %s: upstream deletion returned %d results for %d branches", repository, len(results), len(deletions))
	}
	now := p.now()
	updates := make([]model.BranchDeletionUpdate, 0, len(deletions))
	var abandoned []string
	summary := map[string][]string{}
	finished := 0
	for index, deletion := range deletions {
		result := results[index]
		update := model.BranchDeletionUpdate{ID: deletion.ID, Attempts: deletion.Attempts + 1}
		message := ""
		if result.Err != nil {
			message = result.Err.Error()
		}
		switch result.State {
		case gitcache.UpstreamPropagated:
			update.Status = model.BranchDeletionPropagated
		case gitcache.UpstreamSkipped:
			update.Status, update.Error = model.BranchDeletionSkipped, message
		case gitcache.UpstreamRetry:
			if update.Attempts >= branchDeletionMaxAttempts {
				update.Status = model.BranchDeletionFailed
				update.Error = fmt.Sprintf("all %d attempts failed; last: %s", branchDeletionMaxAttempts, message)
				abandoned = append(abandoned, deletion.Branch)
			} else {
				update.Status, update.Error = model.BranchDeletionPending, message
				update.RetryAfter = now.Add(branchDeletionDelay(update.Attempts, result.Err))
			}
		default:
			update.Status, update.Error = model.BranchDeletionFailed, message
			abandoned = append(abandoned, deletion.Branch)
		}
		if update.Status != model.BranchDeletionPending {
			finished++
		}
		summary[string(update.Status)] = append(summary[string(update.Status)], deletion.Branch)
		updates = append(updates, update)
	}
	// Final local cleanup gets a fresh budget: the network pass may have
	// spent the caller's.
	cleanCtx, cleanCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cleanCancel()
	var errs []error
	if len(abandoned) > 0 {
		if err := p.git.AbandonBranchDeletions(cleanCtx, input, abandoned); err != nil {
			errs = append(errs, fmt.Errorf("restore abandoned branches of %s: %w", repository, err))
		}
	}
	if err := p.store.UpdateBranchDeletions(cleanCtx, updates); err != nil {
		return 0, errors.Join(append(errs, err)...)
	}
	details := map[string]any{"repo": input, "repo_id": deletions[0].RepoID, "attempted": len(deletions)}
	for status, branches := range summary {
		details[status] = len(branches)
		if len(branches) > branchDeletionAuditNames {
			branches = branches[:branchDeletionAuditNames]
		}
		details[status+"_branches"] = branches
	}
	if passErr != nil {
		details["error"] = passErr.Error()
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return finished, errors.Join(append(errs, err)...)
	}
	if _, err := p.auditor.AppendAuditAction(cleanCtx, model.AuditActionSpec{Actor: branchDeletionActor, Action: "branch.delete.upstream",
		ResourceType: "repository", ResourceID: fmt.Sprint(deletions[0].RepoID), Details: string(encoded)}); err != nil {
		errs = append(errs, fmt.Errorf("audit upstream deletion of %s: %w", repository, err))
	}
	return finished, errors.Join(errs...)
}

// branchDeletionDelay backs off 1, 2, 4, 8, 16 minutes. Transient forge
// errors and unclassified ones share the schedule; the attempt cap bounds both.
func branchDeletionDelay(attempt int, err error) time.Duration {
	delay := time.Minute << max(attempt-1, 0)
	if err != nil && !pushretry.Transient(err) && delay < 2*time.Minute {
		delay = 2 * time.Minute
	}
	return min(delay, 30*time.Minute)
}
