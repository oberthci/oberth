package service

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type runQuery struct {
	Repo, ID, SHA, Ref, Action string
	Offset, Limit              int
	Helpers                    bool
	Before, RefGlob            string
	Status                     stepmodel.RunStatus
	Latest, Own, Held          bool
	Until                      string
	Trigger                    model.RunTrigger
}

// PrepareRunGet authorizes and resolves before transport opens a stream.
func (s *Service) prepareRunGet(ctx context.Context, a actor.Actor, q runQuery) (func(context.Context) (stepmodel.RunSummary, error), error) {
	a, err := s.allow(ctx, a, "run_get")
	if err != nil {
		return nil, err
	}
	r, err := s.resolveRun(ctx, a, q)
	if err != nil {
		return nil, err
	}
	return bounded(func(ctx context.Context) (stepmodel.RunSummary, error) { return s.runView(ctx, r, q) }), nil
}

func (s *Service) runView(ctx context.Context, r stateport.Run, q runQuery) (stepmodel.RunSummary, error) {
	steps, err := s.c.Runs.Steps(ctx, r.ID)
	if err != nil {
		return r.RunSummary, err
	}
	for i := range steps {
		if steps[i].DisplayReason == "" {
			reason := stepReason(steps[i].Status)
			steps[i].DisplayReason = reason
		}
		if !q.Helpers {
			steps[i].Logs = slices.DeleteFunc(steps[i].Logs, nativeHelper)
		}
	}
	start := min(q.Offset, len(steps))
	end := start + min(pageLimit(q.Limit, 100), len(steps)-start)
	r.Steps = steps[start:end]
	if end < len(steps) {
		r.NextStepOffset = &end
	}
	return summary(r.RunSummary), nil
}
func (s *Service) listFilter(ctx context.Context, a actor.Actor, q runQuery) (stateport.RunFilter, error) {
	if q.Before != "" && !validRunID(q.Before) {
		return stateport.RunFilter{}, bad("before")
	}
	if q.Status != "" && !q.Status.Valid() {
		return stateport.RunFilter{}, bad("status")
	}
	if !stateport.ValidRefGlob(q.RefGlob) {
		return stateport.RunFilter{}, bad("ref_glob")
	}
	scope, err := s.repositoryScope(ctx, a)
	if err != nil {
		return stateport.RunFilter{}, err
	}
	f := stateport.RunFilter{Repositories: scope, Cursor: q.Before, Ref: q.Ref, RefGlob: q.RefGlob, Status: q.Status, Latest: q.Latest, Limit: pageLimit(q.Limit, 50)}
	if q.Own {
		f.Actor = a.Identity
	}
	if q.Repo != "" {
		r, err := s.resolveRepo(ctx, a, q.Repo)
		if err != nil {
			return f, err
		}
		f.Repositories = []string{r.Canonical}
	}
	return f, nil
}
func (s *Service) prepareRunList(ctx context.Context, a actor.Actor, q runQuery) (func(context.Context) (apicontract.RunPage, error), error) {
	a, err := s.allow(ctx, a, "run_list")
	if err != nil {
		return nil, err
	}
	f, err := s.listFilter(ctx, a, q)
	if err != nil {
		return nil, err
	}
	return bounded(func(ctx context.Context) (apicontract.RunPage, error) {
		if q.Held {
			return s.heldRunPage(ctx, f)
		}
		rows, err := s.c.Runs.Runs(ctx, f)
		page := apicontract.RunPage{Runs: []stepmodel.RunSummary{}}
		for _, r := range rows {
			r.Steps = nil
			r.NextStepOffset = nil
			page.Runs = append(page.Runs, summary(r.RunSummary))
		}
		if len(rows) == f.Limit {
			page.NextCursor = rows[len(rows)-1].ID
		}
		return page, err
	}), nil
}

func (s *Service) prepareStatus(ctx context.Context, a actor.Actor, q runQuery) (func(context.Context) (stepmodel.RunSummary, error), error) {
	a, err := s.allow(ctx, a, "status")
	if err != nil {
		return nil, err
	}
	r, err := s.resolveRun(ctx, a, q)
	if err != nil {
		return nil, err
	}
	return bounded(func(context.Context) (stepmodel.RunSummary, error) { return summary(r.RunSummary), nil }), nil
}

func (s *Service) repos(ctx context.Context, scope []string, options apicontract.RepoListArgs) ([]apicontract.Repository, error) {
	out := []apicontract.Repository{}
	q := stateport.CatalogFilter{Repositories: scope, Limit: stateport.RunPageLimit, IncludeArchived: true}
	for {
		rows, err := s.c.Catalog.Repositories(ctx, q)
		if err != nil {
			return nil, err
		}
		page, err := s.repoViews(ctx, selectArchivedRepositories(rows, options.Archived), options.NamesOnly)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(rows) < q.Limit {
			return out, nil
		}
		q.Before = rows[len(rows)-1].ID
	}
}

func (s *Service) repoViews(ctx context.Context, repos []stateport.Repository, namesOnly bool) ([]apicontract.Repository, error) {
	if namesOnly {
		out := make([]apicontract.Repository, 0, len(repos))
		for _, r := range repos {
			out = append(out, apicontract.Repository{Repo: r.Canonical, Archived: r.Archived})
		}
		return out, nil
	}
	if len(repos) == 0 {
		return []apicontract.Repository{}, nil
	}
	scope := make([]string, len(repos))
	for i, r := range repos {
		scope[i] = r.Canonical
	}
	latest, err := s.c.Runs.Runs(ctx, stateport.RunFilter{Repositories: scope, LatestRepository: true, Limit: stateport.RunPageLimit})
	if err != nil {
		return nil, err
	}
	out := []apicontract.Repository{}
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for _, r := range repos {
		v := apicontract.Repository{Repo: r.Canonical, Archived: r.Archived}
		for _, run := range latest {
			if run.Repo == r.Canonical {
				latest := summary(run.RunSummary)
				v.LatestRun = &latest
				break
			}
		}
		out = append(out, v)
	}
	for i := range out {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil, ctx.Err()
		}
		wg.Add(1)
		go func() { defer wg.Done(); defer func() { <-slots }(); s.branchCount(ctx, &out[i]) }()
	}
	wg.Wait()
	return out, nil
}
func (s *Service) branchCount(ctx context.Context, v *apicontract.Repository) {
	read, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	defaultRef, err := s.c.Source.DefaultBranch(read, v.Repo)
	if err != nil {
		return
	}
	v.DefaultBranch = defaultRef
	refs, err := s.c.Git.ListRefs(read, v.Repo)
	if err != nil {
		return
	}
	n := 0
	for _, r := range refs {
		if strings.HasPrefix(r.Name, "refs/heads/") && r.Name != v.DefaultBranch {
			n++
		}
	}
	v.BranchCount = &n
}
func pageLimit(n, def int) int {
	if n == 0 {
		return def
	}
	return n
}
func nativeHelper(l stepmodel.LogSelector) bool {
	return l.Container == "wait" || l.Container == "init" || l.Container == "oberth-executor-token" || strings.HasPrefix(l.Container, "init-artifact-")
}
