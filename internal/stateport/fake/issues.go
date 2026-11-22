package fake

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func (m *runMemory) projectCI(r stateport.Run, now time.Time) error {
	if r.Origin.HasAssistance() || !runTerminal(r) {
		return runProblem(publicerr.Conflict)
	}
	if seq, ok := m.ciSequences[r.Repo+"\x00"+r.Ref]; ok && seq >= r.Sequence {
		return nil
	}
	return m.recordProjection(r, now)
}

// projectPublished makes a delivered publication's passing run the newest CI
// result of the published ref, ordered after every run that exists now.
func (m *runMemory) projectPublished(p model.Publication, now time.Time) error {
	r, ok := m.runs[p.RunID]
	if !ok {
		return runProblem(publicerr.NotFound)
	}
	if r.Status != stepmodel.RunPassed || r.Origin.HasAssistance() || r.CommitSHA == nil || p.CommitSHA == nil || *r.CommitSHA != *p.CommitSHA {
		return runProblem(publicerr.Conflict)
	}
	if m.ciCounted[r.ID] {
		return nil
	}
	for _, run := range m.runs {
		r.Sequence = max(r.Sequence, run.Sequence)
	}
	r.Ref = p.Ref
	return m.recordProjection(r, now)
}
func (m *runMemory) recordProjection(r stateport.Run, now time.Time) error {
	key := r.Repo + "\x00" + r.Ref
	m.ciCounted[r.ID] = true
	issue := m.ciIssue(r)
	if r.Status == stepmodel.RunPassed && issue.ID == 0 {
		m.ciSequences[key] = r.Sequence
		return nil
	}
	issue = issue.WithCIOutcome(model.CIIssueOutcome{Repo: r.Repo, Ref: r.Ref, RunID: r.ID,
		Body: jsonValue(r.Failure), Passed: r.Status == stepmodel.RunPassed, At: now})
	if issue.ID == 0 {
		m.issueID++
		issue.ID = m.issueID
	}
	if err := m.saveIssue(issue); err != nil {
		return err
	}
	m.ciSequences[key] = r.Sequence
	return nil
}
func (f *Runs) Issue(ctx context.Context, id int64) (model.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "Issue", false); err != nil {
		return model.Issue{}, err
	}
	v, ok := f.data.issues[id]
	if !ok {
		return v, runProblem(publicerr.NotFound)
	}
	return v.Clone(), nil
}
func (f *Runs) Issues(ctx context.Context, scope []string, q model.IssueFilter) ([]model.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.before(ctx, "Issues", false); err != nil {
		return nil, err
	}
	set := []model.Issue{}
	for _, v := range f.data.issues {
		if issueVisible(v, scope) && issueInSet(v, q) {
			set = append(set, v.Clone())
		}
	}
	return issuePage(set, q, f.clock()).Issues, nil
}
func issueVisible(v model.Issue, scope []string) bool {
	if scope == nil {
		return true
	}
	for _, repo := range scope {
		if repo == v.Repo || v.Repo == "" && strings.HasPrefix(repo, v.Upstream+"/") {
			return true
		}
	}
	return false
}
func (f *Runs) CreateIssue(ctx context.Context, a stateport.Action, v model.Issue) (model.Issue, error) {
	a.Events = slices.Clone(a.Events)
	if !a.AllowsIssueUpstream(v.Upstream) {
		return model.Issue{}, runProblem(publicerr.Forbidden)
	}
	err := f.movingChange(ctx, "CreateIssue", a.Events, func(m *runMemory) error {
		if !v.Kind.Valid() || v.Title == "" {
			return runProblem(publicerr.InvalidInput)
		}
		v.Author = a.Actor
		m.issueID++
		v.ID = m.issueID
		v.Revision = 1
		v.State = model.IssueOpen
		v.WorkStatus = model.IssueTodo
		v.Occurrences = 1
		v.CreatedAt = f.clock()
		v.UpdatedAt = v.CreatedAt
		a.ObserveIssue(v)
		if err := m.saveIssue(v); err != nil {
			return err
		}
		m.record(a.Actor, "issue.create", v.Repo, a.Reason)
		return nil
	})
	return v, err
}
func (f *Runs) UpdateIssue(ctx context.Context, a stateport.Action, p model.IssuePatch) (model.Issue, error) {
	a.Events = slices.Clone(a.Events)
	if !a.AllowsIssuePatch(p.Upstream) {
		return model.Issue{}, runProblem(publicerr.Forbidden)
	}
	var out model.Issue
	err := p.Validate()
	if err != nil {
		return out, err
	}
	err = f.movingChange(ctx, "UpdateIssue", a.Events, func(m *runMemory) error {
		v, ok := m.issues[p.ID]
		if !ok {
			return runProblem(publicerr.NotFound)
		}
		v = v.Clone()
		if p.ExpectedRevision != nil && *p.ExpectedRevision != v.Revision {
			return runProblem(publicerr.Conflict)
		}
		if locked(v, a.Actor, f.clock()) {
			return runProblem(publicerr.Conflict)
		}
		v = patchIssue(v, p)
		if v.Title == "" {
			return runProblem(publicerr.InvalidInput)
		}
		v.Revision++
		v.UpdatedAt = f.clock()
		if v.Lock != nil && v.Lock.Owner == a.Actor {
			v.Lock.ExpiresAt = f.clock().Add(5 * time.Minute)
		}
		a.ObserveIssue(v)
		if err := m.saveIssue(v); err != nil {
			return err
		}
		out = v.Clone()
		m.record(a.Actor, "issue.update", v.Repo, a.Reason)
		return nil
	})
	return out, err
}
func locked(v model.Issue, owner string, now time.Time) bool {
	return v.Lock != nil && v.Lock.Owner != owner && v.Lock.ExpiresAt.After(now)
}
func (f *Runs) ChangeIssueLock(ctx context.Context, a stateport.Action, q stateport.IssueLockRequest) error {
	a.Events = slices.Clone(a.Events)
	return f.movingChange(ctx, "ChangeIssueLock", a.Events, func(m *runMemory) error {
		v, ok := m.issues[q.ID]
		if !ok {
			return runProblem(publicerr.NotFound)
		}
		if locked(v, q.Owner, f.clock()) {
			return runProblem(publicerr.Conflict)
		}
		v = v.Clone()
		v.Lock = &model.IssueLock{Owner: q.Owner, AcquiredAt: f.clock(), ExpiresAt: f.clock().Add(5 * time.Minute)}
		a.ObserveIssue(v)
		if err := m.saveIssue(v); err != nil {
			return err
		}
		m.record(a.Actor, "issue.lock", v.Repo, a.Reason)
		return nil
	})
}
func (f *Runs) issueState(ctx context.Context, a stateport.Action, q stateport.IssueClose, action string) error {
	a.Events = slices.Clone(a.Events)
	return f.movingChange(ctx, action, a.Events, func(m *runMemory) error {
		v, ok := m.issues[q.ID]
		if !ok {
			return runProblem(publicerr.NotFound)
		}
		if v.Revision != q.Revision || v.CIWorkID != q.WorkID || locked(v, a.Actor, f.clock()) {
			return runProblem(publicerr.Conflict)
		}
		if action != "CloseIssue" && v.Kind != model.IssueManual {
			return runProblem(publicerr.InvalidTransition)
		}
		if action == "DeleteIssue" {
			if err := a.CheckIssueDeletion(v); err != nil {
				return err
			}
			a.ObserveIssue(v)
			delete(m.issues, v.ID)
		} else {
			v = v.Clone()
			v.State = model.IssueClosed
			v.ClosedAt = new(f.clock())
			if action == "ReopenIssue" {
				v.State = model.IssueOpen
				v.ClosedAt = nil
			}
			v.Revision++
			v.UpdatedAt = f.clock()
			a.ObserveIssue(v)
			if err := m.saveIssue(v); err != nil {
				return err
			}
		}
		m.record(a.Actor, action, v.Repo, a.Reason)
		return nil
	})
}
func (f *Runs) CloseIssue(ctx context.Context, a stateport.Action, q stateport.IssueClose) error {
	return f.issueState(ctx, a, q, "CloseIssue")
}
func (f *Runs) ReopenIssue(ctx context.Context, a stateport.Action, q stateport.IssueClose) error {
	return f.issueState(ctx, a, q, "ReopenIssue")
}
func (f *Runs) DeleteIssue(ctx context.Context, a stateport.Action, q stateport.IssueClose) error {
	return f.issueState(ctx, a, q, "DeleteIssue")
}

func (m *runMemory) ciIssue(r stateport.Run) model.Issue {
	var issue model.Issue
	for _, v := range m.issues {
		if v.Kind == model.IssueCI && v.Repo == r.Repo && v.Branch == r.Ref {
			issue = v.Clone()
			break
		}
	}
	return issue
}

// issueInSet matches the filter's repository, kind and query, the selectors
// that also bound the list's counts.
func issueInSet(v model.Issue, q model.IssueFilter) bool {
	if q.Repo != "" && q.Repo != v.Repo || q.Kind != "" && q.Kind != v.Kind {
		return false
	}
	return q.Query == "" || strings.Contains(strings.ToLower(v.Title+" "+v.Body), strings.ToLower(q.Query))
}

// issueSelected matches the filter's state, work status and stale choice.
func issueSelected(v model.Issue, q model.IssueFilter) bool {
	if q.State != "" && q.State != "all" && q.State != string(v.State) {
		return false
	}
	return (q.WorkStatus == "" || q.WorkStatus == v.WorkStatus) && (!q.Stale || v.Stale)
}

func patchIssue(v model.Issue, p model.IssuePatch) model.Issue {
	if p.Title != nil {
		v.Title = *p.Title
	}
	if p.Body != nil {
		v.Body = *p.Body
	}
	if p.Upstream != nil {
		v.Upstream = *p.Upstream
	}
	if p.WorkStatus != nil {
		v.WorkStatus = *p.WorkStatus
	}
	return v
}

// saveIssue is the single issue write point on the transaction's private snapshot.
func (m *runMemory) saveIssue(v model.Issue) error {
	if err := model.CheckIssueBudget(v); err != nil {
		return err
	}
	m.issues[v.ID] = v.Clone()
	return nil
}
