package api_test

import (
	"context"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
)

// browserNow is the example browser's fixed clock.
func browserNow() time.Time { return time.Date(2026, 11, 16, 20, 22, 0, 0, time.UTC) }

// exampleIssues are the issue list's fixed records, newest last: 64 open
// across the work statuses and 8 closed, in three repositories, one
// upstream and the installation. #70 is an active issue three hours
// untouched, #68 a todo 26 hours untouched (both stale under 24h and 2h),
// and an agent holds #71's lock for three more minutes.
func exampleIssues() []model.Issue {
	titles := []string{"Flaky timeout in the cache warm-up", "Retry the forge push after a refused ref", "Document the release route for tags", "Shorten the admission refusal text", "Index the run list by repository", "Release notes miss the image digest", "Log tail drops the last partial line", "Schedule pause survives an upgrade"}
	places := []struct{ upstream, repo string }{{"origin", "origin/team/api"}, {"origin", "origin/team/web"}, {"mirror", "mirror/tools/cli"}, {"origin", ""}, {"", ""}}
	statuses := []model.IssueWorkStatus{model.IssueTodo, model.IssueTodo, model.IssueReview, model.IssueTodo, model.IssueWaiting, model.IssueTodo, model.IssueReview, model.IssueTodo}
	windows := model.IssueWindows{General: 24 * time.Hour, Active: 2 * time.Hour}
	now, out := browserNow(), []model.Issue{}
	for id := int64(1); id <= 72; id++ {
		place := places[id%5]
		v := model.Issue{ID: id, Revision: 1, Kind: model.IssueManual, Title: titles[id%8], State: model.IssueOpen, WorkStatus: statuses[id%8], Upstream: place.upstream, Repo: place.repo, Occurrences: 1, UpdatedAt: now.Add(-time.Duration(73-id) * 20 * time.Minute)}
		if id%9 == 3 {
			v.State, v.ClosedAt = model.IssueClosed, new(v.UpdatedAt)
		}
		if id%6 == 1 {
			v.Kind, v.Branch, v.Occurrences = model.IssueCI, "main", 3
		}
		out = append(out, v)
	}
	out[69].WorkStatus, out[69].UpdatedAt = model.IssueActive, now.Add(-3*time.Hour)
	out[67].WorkStatus, out[67].UpdatedAt = model.IssueTodo, now.Add(-26*time.Hour)
	out[70].WorkStatus, out[70].Lock = model.IssueActive, &model.IssueLock{Owner: "agent-b", AcquiredAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(3 * time.Minute)}
	out[41].Title, out[41].WorkStatus, out[41].Lock = "Flaky test in pkg/check", model.IssueActive, &model.IssueLock{Owner: "editor", ExpiresAt: now.Add(5 * time.Minute)}
	for i := range out {
		out[i].CreatedAt = out[i].UpdatedAt.Add(-time.Hour)
		out[i].Stale = windows.Stale(out[i], now)
	}
	return out
}

// exampleIssueList pages the fixed issues as issue_list does: counts over the
// query's matches, the chosen filter newest first, and a cursor each way.
func exampleIssueList(issues []model.Issue) apicontract.HandlerFunc[apicontract.IssueListArgs, apicontract.IssuePage] {
	return func(_ context.Context, _ apicontract.Caller, q apicontract.IssueListArgs) (apicontract.Call[apicontract.IssuePage], error) {
		return func(context.Context) (apicontract.IssuePage, error) {
			page, selected := apicontract.IssuePage{Issues: []model.Issue{}}, []model.Issue{}
			for i := len(issues) - 1; i >= 0; i-- {
				v := issues[i]
				if !strings.Contains(strings.ToLower(v.Title), strings.ToLower(q.Query)) {
					continue
				}
				exampleCount(&page.Counts, v)
				if (q.State == "closed") == (v.State == model.IssueClosed) && (q.WorkStatus == "" || q.WorkStatus == string(v.WorkStatus)) && (!q.Stale || v.Stale) {
					selected = append(selected, v)
				}
			}
			examplePage(&page, selected, q)
			return page, nil
		}, nil
	}
}

// examplePage cuts the newest-first selection at the request's cursor.
func examplePage(page *apicontract.IssuePage, selected []model.Issue, q apicontract.IssueListArgs) {
	start, end := 0, len(selected)
	for i, v := range selected {
		if q.Before > 0 && v.ID >= q.Before {
			start = i + 1
		}
		if q.After > 0 && v.ID > q.After {
			end = i + 1
		}
	}
	if q.After > 0 {
		start = max(0, end-q.Limit)
	}
	page.Issues = append(page.Issues, selected[start:min(end, start+q.Limit)]...)
	page.Total, page.Offset = len(selected), start
	if n := len(page.Issues); n > 0 && start > 0 {
		page.NextAfter = page.Issues[0].ID
	}
	if n := len(page.Issues); n > 0 && start+n < len(selected) {
		page.NextBefore = page.Issues[n-1].ID
	}
}

func exampleCount(c *model.IssueCounts, v model.Issue) {
	if v.State == model.IssueClosed {
		c.Closed++
		return
	}
	c.Open++
	*map[model.IssueWorkStatus]*int{model.IssueTodo: &c.Todo, model.IssueActive: &c.Active, model.IssueWaiting: &c.Waiting, model.IssueReview: &c.Review}[v.WorkStatus]++
	if v.Stale {
		c.Stale++
	}
}
