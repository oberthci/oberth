package fake

import (
	"context"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"slices"
	"sort"
)

func issueInScope(v model.Issue, scope stateport.IssueScope) bool {
	if v.Upstream == "" || v.Upstream == "global" {
		return scope.Global
	}
	return scope.Upstreams == nil || slices.Contains(scope.Upstreams, v.Upstream)
}
func (f *Runs) ScopedIssue(ctx context.Context, scope stateport.IssueScope, id int64) (model.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "ScopedIssue", false); err != nil {
		return model.Issue{}, err
	}
	v, ok := f.data.issues[id]
	if !ok || !issueInScope(v, scope) {
		return model.Issue{}, runProblem(publicerr.NotFound)
	}
	return v.Clone(), nil
}
func (f *Runs) ScopedIssues(ctx context.Context, scope stateport.IssueScope, q model.IssueFilter) ([]model.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "ScopedIssues", false); err != nil {
		return nil, err
	}
	out := []model.Issue{}
	for _, v := range f.data.issues {
		if issueInScope(v, scope) && issueMatches(v, nil, q) {
			out = append(out, v.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}
