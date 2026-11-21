package fake

import (
	"context"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"slices"
	"sort"
	"time"
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
func (f *Runs) ScopedIssues(ctx context.Context, scope stateport.IssueScope, q model.IssueFilter) (stateport.IssuePage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "ScopedIssues", false); err != nil {
		return stateport.IssuePage{}, err
	}
	if q.Limit < 1 || q.Before < 0 || q.After < 0 || q.Before > 0 && q.After > 0 {
		return stateport.IssuePage{}, runProblem(publicerr.InvalidInput)
	}
	set := []model.Issue{}
	for _, v := range f.data.issues {
		if issueInScope(v, scope) && issueInSet(v, q) {
			set = append(set, v.Clone())
		}
	}
	return issuePage(set, q, f.clock()), nil
}

// issuePage pages an issue set as the store does: newest first, one cursor,
// and totals over the same set observed at now.
func issuePage(set []model.Issue, q model.IssueFilter, now time.Time) stateport.IssuePage {
	sort.Slice(set, func(i, j int) bool { return set[i].ID > set[j].ID })
	page := stateport.IssuePage{Issues: []model.Issue{}}
	selected := []model.Issue{}
	for _, v := range set {
		v.Stale = q.Windows.Stale(v, now)
		countIssue(&page.Counts, v)
		if issueSelected(v, q) {
			selected = append(selected, v)
		}
	}
	page.Total = len(selected)
	page.Issues = append(page.Issues, issueWindow(selected, q)...)
	newer := q.After
	switch {
	case len(page.Issues) > 0:
		newer = page.Issues[0].ID
	case q.Before > 0:
		newer = q.Before - 1
	}
	for _, v := range selected {
		if v.ID > newer {
			page.Offset++
		}
	}
	return page
}

// issueWindow is the page of a newest-first selection at the filter's cursor.
func issueWindow(selected []model.Issue, q model.IssueFilter) []model.Issue {
	start, end := 0, len(selected)
	for i, v := range selected {
		if q.After > 0 && v.ID <= q.After {
			end = i
			break
		}
		if q.Before > 0 && v.ID >= q.Before {
			start = i + 1
		}
	}
	if q.After > 0 {
		return selected[max(start, end-q.Limit):end]
	}
	return selected[start:min(end, start+q.Limit)]
}

func countIssue(c *model.IssueCounts, v model.Issue) {
	if v.State != model.IssueOpen {
		c.Closed++
		return
	}
	c.Open++
	for status, count := range map[model.IssueWorkStatus]*int{model.IssueTodo: &c.Todo, model.IssueActive: &c.Active, model.IssueWaiting: &c.Waiting, model.IssueReview: &c.Review} {
		if v.WorkStatus == status {
			*count++
		}
	}
	if v.Stale {
		c.Stale++
	}
}
