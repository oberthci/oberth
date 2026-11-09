package service

import (
	"context"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type repoSummary struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Upstream      string     `json:"upstream"`
	DefaultBranch string     `json:"default_branch"`
	ArchivedAt    *time.Time `json:"archived_at"`
	// NonDefaultBranches is the mirror's branch count besides the default
	// branch (#1508); null when unknown. 0 everywhere means no leftovers.
	NonDefaultBranches *int           `json:"non_default_branches"`
	LatestRun          *repoLatestRun `json:"latest_run"`
}
type repoLatestRun struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	Ref        string     `json:"ref"`
	FinishedAt *time.Time `json:"finished_at"`
}

// repoListArchived selects archived repositories: the default "exclude" keeps
// retired repositories out of everyday catalog reads (#1498); "include" returns
// the whole catalog and "only" the archived ones. History stays readable.
func repoListArchived(value string) (string, error) {
	switch value {
	case "":
		return "exclude", nil
	case "exclude", "include", "only":
		return value, nil
	default:
		return "", fmt.Errorf("%w: archived must be exclude, include or only", ErrInvalidInput)
	}
}

// Called on the actor-scoped service; the dashboard retains its history view.
func (service *API) repoList(ctx context.Context, archived string) (api.RepoListResponse, error) {
	archived, err := repoListArchived(archived)
	if err != nil {
		return api.RepoListResponse{}, err
	}
	var repos []model.Repository
	if service.repositories != nil {
		repos, err = service.repositories.ListRepositories(ctx)
	} else {
		repos, err = service.runs.ListRepositories(ctx)
	}
	if err != nil {
		return api.RepoListResponse{}, err
	}
	latest := map[int64]model.Run{}
	if service.history != nil {
		runs, historyErr := service.history.ListLatestRunsPerRepo(ctx, 1)
		if historyErr != nil {
			return api.RepoListResponse{}, fmt.Errorf("list latest repository runs: %w", historyErr)
		}
		for _, run := range runs {
			latest[run.RepoID] = run
		}
	}
	listed := make([]model.Repository, 0, len(repos))
	hidden := 0
	for _, repo := range repos {
		if (archived == "exclude" && repo.ArchivedAt != nil) || (archived == "only" && repo.ArchivedAt == nil) {
			if repo.ArchivedAt != nil {
				hidden++
			}
			continue
		}
		listed = append(listed, repo)
	}
	// Count only what is listed, so hidden archived mirrors cost nothing.
	branches := service.nonDefaultBranchCounts(ctx, listed)
	summaries := make([]repoSummary, 0, len(listed))
	for _, repo := range listed {
		summary := repoSummary{ID: repo.ID, Name: repo.Name, DefaultBranch: repo.DefaultBranch, ArchivedAt: repo.ArchivedAt, NonDefaultBranches: branchCountPointer(branches, repo.ID)}
		if resolver, ok := service.runs.(upstreamResolver); ok {
			upstream, err := resolver.Upstream(ctx, repo.UpstreamID)
			if err != nil {
				return api.RepoListResponse{}, err
			}
			summary.Upstream = upstream.Name
		}
		if run, ok := latest[repo.ID]; ok {
			shown := service.presentationRun(ctx, run)
			summary.LatestRun = &repoLatestRun{ID: run.ID, Status: shown.DisplayStatus, Ref: run.Ref, FinishedAt: run.FinishedAt}
		}
		summaries = append(summaries, summary)
	}
	return api.RepoListResponse{Repositories: summaries, ArchivedHidden: hidden}, nil
}
