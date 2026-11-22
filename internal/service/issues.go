package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/stateport"
)

const issueBytes = model.IssueBytes

func (s *Service) issue(ctx context.Context, a actor.Actor, id int64) (model.Issue, error) {
	v, err := s.c.People.Issues.ScopedIssue(ctx, issueReadScope(a), id)
	if isMissing(err) {
		return model.Issue{}, refuse(issueMissing)
	}
	return v, err
}
func issueReadScope(a actor.Actor) stateport.IssueScope {
	scope := a.Scope
	if len(scope) == 0 {
		scope = nil
	}
	return stateport.IssueScope{Upstreams: scope, Global: a.Admin && len(a.Scope) == 0}
}

// issueWindows resolves a read's stale windows. An explicit stale_after sets
// the general window, and the active one too unless active_stale_after is
// given; a window the caller omits is the installation's.
func (s *Service) issueWindows(stale, active string) (model.IssueWindows, error) {
	w := s.c.People.IssueWindows
	general, err := issueWindow(stale, w.General, "stale_after")
	if err != nil {
		return w, err
	}
	if stale != "" {
		w.Active = general
	}
	w.General = general
	w.Active, err = issueWindow(active, w.Active, "active_stale_after")
	return w, err
}
func issueWindow(raw string, fallback time.Duration, field string) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, bad(field)
	}
	return d, nil
}

// withoutExpiredLock observes an expired lock as absent; it never writes.
func withoutExpiredLock(v model.Issue, now time.Time) model.Issue {
	if v.Lock != nil && !now.Before(v.Lock.ExpiresAt) {
		v.Lock = nil
	}
	return v
}
func (s *Service) PrepareIssueGet(ctx context.Context, c apicontract.Caller, q apicontract.IssueGetArgs) (apicontract.Call[model.Issue], error) {
	a, err := s.peopleAllow(ctx, c.Actor, "issue_get")
	if err != nil {
		return nil, err
	}
	windows, err := s.issueWindows(q.StaleAfter, q.ActiveStaleAfter)
	if err != nil {
		return nil, err
	}
	if _, err = s.issue(ctx, a, q.ID); err != nil {
		return nil, err
	}
	return bounded(func(ctx context.Context) (model.Issue, error) {
		v, e := s.issue(ctx, a, q.ID)
		if e != nil {
			return v, e
		}
		now := s.c.Now()
		v.Stale = windows.Stale(v, now)
		v = withoutExpiredLock(v, now)
		raw, e := json.Marshal(v)
		if e == nil && len(raw) > issueBytes {
			return model.Issue{}, refuse(issueTooLarge)
		}
		return v, e
	}), nil
}
func (s *Service) PrepareIssueGetMany(ctx context.Context, c apicontract.Caller, q apicontract.IssueGetManyArgs) (apicontract.Call[apicontract.IssueManyResult], error) {
	if _, err := s.peopleAllow(ctx, c.Actor, "issue_get_many"); err != nil {
		return nil, err
	}
	ids := slices.Clone(q.IDs)
	return bounded(func(ctx context.Context) (apicontract.IssueManyResult, error) {
		out := apicontract.IssueManyResult{Issues: []apicontract.IssueResult{}}
		remaining := issueBytes - 32 - len(ids)*512
		if len(ids) == 1 {
			remaining = issueBytes
		}
		for _, id := range ids {
			call, err := s.PrepareIssueGet(ctx, c, apicontract.IssueGetArgs{ID: id, StaleAfter: q.StaleAfter, ActiveStaleAfter: q.ActiveStaleAfter})
			v := model.Issue{}
			if err == nil {
				v, err = call(ctx)
			}
			row := apicontract.IssueResult{ID: id}
			if err != nil {
				row.Error = publicIssueError(err)
			} else {
				raw, _ := json.Marshal(v)
				if len(raw) > remaining {
					row.Error = issueReadAlone(id)
				} else {
					remaining -= len(raw)
					row.Issue = &v
				}
			}
			out.Issues = append(out.Issues, row)
		}
		return out, nil
	}), nil
}
func (s *Service) PrepareIssueList(ctx context.Context, c apicontract.Caller, q apicontract.IssueListArgs) (apicontract.Call[apicontract.IssuePage], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "issue_list")
	if e != nil {
		return nil, e
	}
	scope := issueReadScope(a)
	if q.Repo != "" {
		r, err := s.resolveRepo(ctx, a, q.Repo)
		if err != nil {
			return nil, err
		}
		q.Repo = r.Canonical
	}
	f := model.IssueFilter{Repo: q.Repo, Kind: model.IssueKind(q.Kind), State: q.State, WorkStatus: model.IssueWorkStatus(q.WorkStatus), Query: q.Query, Stale: q.Stale, Before: q.Before, After: q.After, Limit: q.Limit}
	if f.Limit == 0 {
		f.Limit = 20
	}
	if f.State == "" {
		f.State = "open"
	}
	windows, err := s.issueWindows(q.StaleAfter, q.ActiveStaleAfter)
	if err = errors.Join(issueSelectors(f), err); err != nil {
		return nil, err
	}
	f.Windows = windows
	return bounded(func(ctx context.Context) (apicontract.IssuePage, error) { return s.issuePage(ctx, scope, f) }), nil
}

// issuePage returns metadata rows with their totals; each cursor is offered
// only when issues lie beyond the page in its direction.
func (s *Service) issuePage(ctx context.Context, scope stateport.IssueScope, f model.IssueFilter) (apicontract.IssuePage, error) {
	page, err := s.c.People.Issues.ScopedIssues(ctx, scope, f)
	out := apicontract.IssuePage{Issues: []model.Issue{}, Counts: page.Counts, Total: page.Total, Offset: page.Offset}
	if err != nil {
		return out, err
	}
	now := s.c.Now()
	for _, v := range page.Issues {
		v.Body = ""
		v.History = nil
		out.Issues = append(out.Issues, withoutExpiredLock(v, now))
	}
	if n := len(out.Issues); n > 0 {
		if page.Offset > 0 {
			out.NextAfter = out.Issues[0].ID
		}
		if page.Offset+n < page.Total {
			out.NextBefore = out.Issues[n-1].ID
		}
	}
	return out, nil
}

func (s *Service) PrepareIssueCreate(ctx context.Context, c apicontract.Caller, q apicontract.IssueCreateArgs) (apicontract.Call[apicontract.IssueResult], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "issue_create")
	if e != nil {
		return nil, e
	}
	if q.Upstream != "" {
		if e = issueScope(a, q.Upstream); e != nil {
			return nil, e
		}
	}
	if q.Body == nil || strings.TrimSpace(q.Title) == "" {
		return nil, bad("title/body")
	}
	if len(*q.Body)+len(q.Title) > issueBytes {
		return nil, refuse(issueTooLarge)
	}
	body := *q.Body
	return bounded(func(ctx context.Context) (apicontract.IssueResult, error) {
		upstream, err := s.issueUpstream(ctx, a, q.Upstream)
		if err != nil {
			return apicontract.IssueResult{}, err
		}
		v, err := s.c.People.Issues.CreateIssue(ctx, s.peopleAction(a, "issue.created", q.Reason, ""), model.Issue{Kind: model.IssueManual, Title: q.Title, Body: body, Upstream: upstream})
		return issueResult(v), err
	}), nil
}
func issueScope(a actor.Actor, upstream string) error {
	if reponame.ValidateUpstreamName(upstream) != nil {
		return bad("upstream")
	}
	if len(a.Scope) > 0 && !slices.Contains(a.Scope, upstream) {
		return refuse(missing)
	}
	return nil
}
func issueResult(v model.Issue) apicontract.IssueResult {
	return apicontract.IssueResult{ID: v.ID, State: v.State, UpdatedAt: &v.UpdatedAt, Occurrences: v.Occurrences, Issue: &v}
}
func (s *Service) PrepareIssueUpdate(ctx context.Context, c apicontract.Caller, q apicontract.IssueUpdateArgs) (apicontract.Call[apicontract.IssueResult], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "issue_update")
	if e != nil {
		return nil, e
	}
	if _, e = s.issue(ctx, a, q.ID); e != nil {
		return nil, e
	}
	if q.Upstream != nil {
		if !a.Admin || len(a.Scope) > 0 {
			return nil, refuse(installation)
		}
		if e = issueScope(a, *q.Upstream); e != nil {
			return nil, e
		}
	}
	p := model.IssuePatch{ID: q.ID, ExpectedRevision: q.ExpectedRevision, Title: q.Title, Body: q.Body, Upstream: q.Upstream}
	if q.WorkStatus != nil {
		v := model.IssueWorkStatus(*q.WorkStatus)
		p.WorkStatus = &v
	}
	p = p.Clone()
	return bounded(func(ctx context.Context) (apicontract.IssueResult, error) {
		if p.Upstream != nil {
			if _, err := s.requireUpstreams(ctx, []string{*p.Upstream}); err != nil {
				return apicontract.IssueResult{}, err
			}
		}
		v, err := s.c.People.Issues.UpdateIssue(ctx, s.peopleAction(a, "issue.updated", q.Reason, ""), p)
		return issueResult(v), err
	}), nil
}
func (s *Service) PrepareIssueAppend(ctx context.Context, c apicontract.Caller, q apicontract.IssueAppendArgs) (apicontract.Call[apicontract.IssueResult], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "issue_append")
	if e != nil {
		return nil, e
	}
	return bounded(func(ctx context.Context) (apicontract.IssueResult, error) {
		v, err := s.issue(ctx, a, q.ID)
		if err != nil {
			return apicontract.IssueResult{}, err
		}
		body := v.Body + q.Text
		v, err = s.c.People.Issues.UpdateIssue(ctx, s.peopleAction(a, "issue.updated", q.Reason, v.Repo), model.IssuePatch{ID: q.ID, ExpectedRevision: &v.Revision, Body: &body})
		out := issueResult(v)
		out.AppendedBytes = len(q.Text)
		return out, err
	}), nil
}

type issueAction struct {
	ID             int64
	Action, Reason string
}

func (s *Service) issueTransition(ctx context.Context, c apicontract.Caller, q issueAction) (apicontract.Call[apicontract.IssueResult], error) {
	a, e := s.peopleAllow(ctx, c.Actor, q.Action)
	if e != nil {
		return nil, e
	}
	if _, e = s.issue(ctx, a, q.ID); e != nil {
		return nil, e
	}
	return bounded(func(ctx context.Context) (apicontract.IssueResult, error) {
		v, err := s.issue(ctx, a, q.ID)
		if err != nil {
			return apicontract.IssueResult{}, err
		}
		// Runs own CI issues; an administrator may still close one, which is
		// how an issue that no later run will close is cleared.
		if v.Kind == model.IssueCI && (q.Action != "issue_close" || !a.Admin) {
			return apicontract.IssueResult{}, refuse(ciIssueManaged)
		}
		request := stateport.IssueClose{ID: q.ID, Revision: v.Revision, WorkID: v.CIWorkID}
		fact := s.peopleAction(a, "issue.updated", q.Reason, v.Repo)
		switch q.Action {
		case "issue_close":
			err = s.c.People.Issues.CloseIssue(ctx, fact, request)
		case "issue_reopen":
			err = s.c.People.Issues.ReopenIssue(ctx, fact, request)
		case "issue_delete":
			err = s.c.People.Issues.DeleteIssue(ctx, fact, request)
		}
		if err != nil {
			return apicontract.IssueResult{}, err
		}
		if q.Action == "issue_delete" {
			return apicontract.IssueResult{ID: q.ID, Deleted: true}, nil
		}
		v, err = s.issue(ctx, a, q.ID)
		return issueResult(v), err
	}), nil
}
func (s *Service) PrepareIssueClose(ctx context.Context, c apicontract.Caller, q apicontract.IssueCloseArgs) (apicontract.Call[apicontract.IssueResult], error) {
	return s.issueTransition(ctx, c, issueAction{q.ID, "issue_close", q.Reason})
}
func (s *Service) PrepareIssueReopen(ctx context.Context, c apicontract.Caller, q apicontract.IssueReopenArgs) (apicontract.Call[apicontract.IssueResult], error) {
	return s.issueTransition(ctx, c, issueAction{q.ID, "issue_reopen", q.Reason})
}
func (s *Service) PrepareIssueDelete(ctx context.Context, c apicontract.Caller, q apicontract.IssueDeleteArgs) (apicontract.Call[apicontract.IssueResult], error) {
	return s.issueTransition(ctx, c, issueAction{q.ID, "issue_delete", q.Reason})
}
func (s *Service) PrepareIssueLock(ctx context.Context, c apicontract.Caller, q apicontract.IssueLockArgs) (apicontract.Call[model.IssueLock], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "issue_lock")
	if e != nil {
		return nil, e
	}
	if _, e = s.issue(ctx, a, q.ID); e != nil {
		return nil, e
	}
	return bounded(func(ctx context.Context) (model.IssueLock, error) {
		err := s.c.People.Issues.ChangeIssueLock(ctx, s.peopleAction(a, "issue.updated", q.Reason, ""), stateport.IssueLockRequest{ID: q.ID, Owner: a.Identity})
		if err != nil {
			return model.IssueLock{}, err
		}
		v, err := s.issue(ctx, a, q.ID)
		if err != nil {
			return model.IssueLock{}, err
		}
		if v.Lock == nil {
			return model.IssueLock{}, refuse(unavailable)
		}
		return *v.Lock, nil
	}), nil
}

func issueSelectors(f model.IssueFilter) error {
	var errs []error
	if f.Kind != "" && !f.Kind.Valid() {
		errs = append(errs, bad("kind"))
	}
	if f.State != "all" && !model.IssueState(f.State).Valid() {
		errs = append(errs, bad("state"))
	}
	if f.WorkStatus != "" && !f.WorkStatus.Valid() {
		errs = append(errs, bad("work_status"))
	}
	if f.Before > 0 && f.After > 0 {
		errs = append(errs, bad("after"))
	}
	return errors.Join(errs...)
}
