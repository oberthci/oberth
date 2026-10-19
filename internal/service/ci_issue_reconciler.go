package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

const ciIssueReconcileInterval = 5 * time.Minute

const ciIssueReconcilerActor = "oberth:ci-issue-reconciler"

type CIIssueReconcileStore interface {
	ListIssues(context.Context, model.IssueListFilter) (model.IssuePage, error)
	Repository(context.Context, int64) (model.Repository, error)
	Upstream(context.Context, int64) (model.Upstream, error)
	ResolveRunByRefKind(context.Context, int64, string, model.RefKind) (model.Run, error)
	CIBranchWasPublishedOrDeleted(context.Context, int64, string) (bool, error)
	AutoCloseCIIssue(context.Context, string, model.Issue, string) (bool, error)
}

type CIIssueReconcileGit interface {
	Ensure(context.Context, string) (gitcache.Repository, error)
	RemoteRef(context.Context, string, string) (string, bool, error)
	RefSHA(context.Context, string, string) (string, error)
	ReleaseReachable(context.Context, string, string, string) (bool, error)
}

type CIIssueReconciler struct {
	store CIIssueReconcileStore
	git   CIIssueReconcileGit
	gate  func(context.Context) error
	log   *log.Logger
}

func NewCIIssueReconciler(database CIIssueReconcileStore, git CIIssueReconcileGit, gate func(context.Context) error, logger *log.Logger) (*CIIssueReconciler, error) {
	if database == nil || git == nil || gate == nil || logger == nil {
		return nil, errors.New("service: CI issue reconciler requires store, Git, mutation gate, and logger")
	}
	return &CIIssueReconciler{store: database, git: git, gate: gate, log: logger}, nil
}

func (r *CIIssueReconciler) Run(ctx context.Context) error {
	if err := r.Tick(ctx); err != nil && ctx.Err() == nil {
		r.log.Printf("CI issue reconciliation: %v", err)
	}
	ticker := time.NewTicker(ciIssueReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.Tick(ctx); err != nil && ctx.Err() == nil {
				r.log.Printf("CI issue reconciliation: %v", err)
			}
		}
	}
}

// Tick checks current upstream refs, so external merges and deletions converge
// without needing a receive event at this server. Each issue close is guarded
// by its observed projection version in the store.
func (r *CIIssueReconciler) Tick(ctx context.Context) error {
	var before int64
	var problems []error
	for {
		page, err := r.store.ListIssues(ctx, model.IssueListFilter{
			Kind: model.IssueCI, State: model.IssueOpen, BeforeID: before, Limit: 50,
		})
		if err != nil {
			return fmt.Errorf("list open CI issues: %w", err)
		}
		for _, issue := range page.Issues {
			if err := r.reconcileIssue(ctx, issue); err != nil {
				problems = append(problems, fmt.Errorf("issue #%d: %w", issue.ID, err))
			}
		}
		if page.NextBefore == 0 {
			break
		}
		before = page.NextBefore
	}
	return errors.Join(problems...)
}

func (r *CIIssueReconciler) reconcileIssue(ctx context.Context, issue model.Issue) error {
	repo, err := r.store.Repository(ctx, issue.RepoID)
	if err != nil {
		return err
	}
	// A default-branch issue is owned only by its own green CI projection.
	if issue.Branch == repo.DefaultBranch {
		return nil
	}
	upstream, err := r.store.Upstream(ctx, repo.UpstreamID)
	if err != nil {
		return err
	}
	qualified := upstream.QualifiedRepo(repo.Name)
	if _, err := r.git.Ensure(ctx, qualified); err != nil {
		return fmt.Errorf("refresh upstream: %w", err)
	}
	_, exists, err := r.git.RemoteRef(ctx, qualified, "refs/heads/"+issue.Branch)
	if err != nil {
		return fmt.Errorf("read issue branch: %w", err)
	}
	if !exists {
		known, err := r.store.CIBranchWasPublishedOrDeleted(ctx, repo.ID, issue.Branch)
		if err != nil {
			return err
		}
		if !known {
			return nil // a first red branch may never have been published
		}
		return r.closeIssue(ctx, issue, "branch deleted upstream")
	}
	mainRun, err := r.store.ResolveRunByRefKind(ctx, repo.ID, repo.DefaultBranch, model.RefBranch)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load latest default-branch run: %w", err)
	}
	if mainRun.Status != model.RunPassed {
		return nil
	}
	// The accepted public ref may be ahead of the last published upstream
	// ref while a red run is awaiting review. Test its actual head, not the
	// older upstream copy, before treating the issue as merged.
	head, err := r.git.RefSHA(ctx, qualified, issue.Branch)
	if err != nil {
		return fmt.Errorf("read current issue branch head: %w", err)
	}
	mainHead, exists, err := r.git.RemoteRef(ctx, qualified, "refs/heads/"+repo.DefaultBranch)
	if err != nil {
		return fmt.Errorf("read default branch: %w", err)
	}
	if !exists {
		return nil
	}
	merged, err := r.git.ReleaseReachable(ctx, qualified, head, mainHead)
	if err != nil {
		return fmt.Errorf("check issue branch ancestry: %w", err)
	}
	if !merged {
		return nil
	}
	return r.closeIssue(ctx, issue, fmt.Sprintf("branch merged into %s at %s; latest default-branch run %s passed", repo.DefaultBranch, mainHead, mainRun.ID))
}

func (r *CIIssueReconciler) closeIssue(ctx context.Context, issue model.Issue, resolution string) error {
	if err := r.gate(ctx); err != nil {
		return fmt.Errorf("check mutation gate: %w", err)
	}
	_, err := r.store.AutoCloseCIIssue(ctx, ciIssueReconcilerActor, issue, resolution)
	return err
}
