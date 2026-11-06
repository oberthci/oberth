package service

import (
	"context"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type repoSummary struct {
	ID            int64          `json:"id"`
	Name          string         `json:"name"`
	Upstream      string         `json:"upstream"`
	DefaultBranch string         `json:"default_branch"`
	ArchivedAt    *time.Time     `json:"archived_at"`
	LatestRun     *repoLatestRun `json:"latest_run"`
}
type repoLatestRun struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	Ref        string     `json:"ref"`
	FinishedAt *time.Time `json:"finished_at"`
}

// Called on the actor-scoped service; the dashboard retains its history view.
func (service *API) repoList(ctx context.Context) (api.RepoListResponse, error) {
	var repos []model.Repository
	var err error
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
	summaries := make([]repoSummary, len(repos))
	for i, repo := range repos {
		summary := repoSummary{ID: repo.ID, Name: repo.Name, DefaultBranch: repo.DefaultBranch, ArchivedAt: repo.ArchivedAt}
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
		summaries[i] = summary
	}
	return api.RepoListResponse{Repositories: summaries}, nil
}
