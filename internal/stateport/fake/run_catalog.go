package fake

import (
	"context"
	"slices"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func (f *Runs) RepositoryScope(ctx context.Context, upstreams []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if upstreams == nil {
		return nil, nil
	}
	out := []string{}
	for _, r := range f.data.repos {
		if slices.Contains(upstreams, r.Upstream) {
			out = append(out, r.Canonical)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (f *Runs) Repository(ctx context.Context, name string, scope []string) (stateport.Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return stateport.Repository{}, err
	}
	var found stateport.Repository
	count := 0
	for _, r := range f.data.repos {
		if visibleRun(scope, r.Canonical) && (r.Canonical == name || r.Name == name) {
			found = r
			count++
		}
	}
	if count == 0 {
		return found, runProblem(publicerr.NotFound)
	}
	if count > 1 {
		return found, runProblem(publicerr.Ambiguous)
	}
	return found, nil
}

func (f *Runs) Repositories(ctx context.Context, q stateport.CatalogFilter) ([]stateport.Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q.Limit < 1 || q.Limit > stateport.RunPageLimit || q.Before < 0 {
		return nil, runProblem(publicerr.InvalidInput)
	}
	out := []stateport.Repository{}
	for _, r := range f.data.repos {
		if visibleRun(q.Repositories, r.Canonical) && (q.IncludeArchived || !r.Archived) && (q.Before == 0 || r.ID < q.Before) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b stateport.Repository) int {
		if a.ID > b.ID {
			return -1
		}
		return 1
	})
	return out[:min(len(out), q.Limit)], nil
}

func (f *Runs) Steps(ctx context.Context, id string) ([]stepmodel.RunStep, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, ok := f.data.runs[id]
	if !ok {
		return nil, runProblem(publicerr.NotFound)
	}
	if r.Steps != nil {
		return r.RunSummary.Clone().Steps, nil
	}
	return stateport.ProjectSteps(r, f.data.native[id].Stored), nil
}
