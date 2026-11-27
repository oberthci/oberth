package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func (s *Store) CreateIssue(ctx context.Context, action stateport.Action, issue model.Issue) (model.Issue, error) {
	action.Events = slices.Clone(action.Events)
	if !action.AllowsIssueUpstream(issue.Upstream) {
		return model.Issue{}, refuse(forbidden, nil)
	}
	if !issue.Kind.Valid() || issue.Title == "" {
		return model.Issue{}, refuse(invalid, nil)
	}
	issue.Author = action.Actor
	issue.ID = 0
	issue.Revision = 1
	issue.Lock = nil
	issue.State = model.IssueOpen
	issue.WorkStatus = model.IssueTodo
	issue.Occurrences = 1
	issue.ClosedAt = nil
	issue.CreatedAt = s.options.Clock().UTC()
	issue.UpdatedAt = issue.CreatedAt
	err := s.change(ctx, mutation{action.Actor, "issue.create", "issue", "", issue.Title}, func(tx *sql.Tx) error {
		if err := s.saveIssue(ctx, tx, &issue, true); err != nil {
			return err
		}
		action.ObserveIssue(issue)
		return nil
	}, action.Events...)
	return issue, err
}

func readIssue(ctx context.Context, tx *sql.Tx, id int64) (model.Issue, error) {
	var body string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM issues WHERE id=?`, id).Scan(&body); err != nil {
		return model.Issue{}, databaseError(err)
	}
	var issue model.Issue
	if err := json.Unmarshal([]byte(body), &issue); err != nil {
		return issue, refuse(corrupt, err)
	}
	return issue, nil
}

func (s *Store) saveIssue(ctx context.Context, tx *sql.Tx, issue *model.Issue, insert bool) error {
	if insert && issue.ID == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT coalesce((SELECT seq+1 FROM sqlite_sequence WHERE name='issues'),1)`).Scan(&issue.ID); err != nil {
			return databaseError(err)
		}
	}
	if err := model.CheckIssueBudget(*issue); err != nil {
		return err
	}
	query := `UPDATE issues SET revision=?,upstream=?,repo=?,state=?,value=? WHERE id=?`
	args := []any{issue.Revision, issue.Upstream, issue.Repo, issue.State, encode(*issue), issue.ID}
	if insert {
		query = `INSERT INTO issues(revision,upstream,repo,state,value,id) VALUES(?,?,?,?,?,?)`
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return databaseError(err)
	}
	s.noteIssueWrite(issue.ID)
	s.noteChange(stateport.Change{Repository: issue.Repo, Upstream: issue.Upstream})
	return nil
}

func (s *Store) Issue(ctx context.Context, id int64) (model.Issue, error) {
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.Issue{}, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	return readIssue(ctx, tx, id)
}

func (s *Store) UpdateIssue(ctx context.Context, action stateport.Action, patch model.IssuePatch) (model.Issue, error) {
	action.Events = slices.Clone(action.Events)
	if !action.AllowsIssuePatch(patch.Upstream) {
		return model.Issue{}, refuse(forbidden, nil)
	}
	if err := patch.Validate(); err != nil {
		return model.Issue{}, err
	}
	var issue model.Issue
	err := s.change(ctx, mutation{action.Actor, "issue.update", "issue", formatID(patch.ID), nil}, func(tx *sql.Tx) error {
		var err error
		issue, err = readIssue(ctx, tx, patch.ID)
		if err != nil {
			return err
		}
		if err := requireIssueVersion(issue, patch); err != nil {
			return err
		}
		now := s.options.Clock().UTC()
		if issue.Lock != nil && issue.Lock.ExpiresAt.After(now) && issue.Lock.Owner != action.Actor {
			return refuse(lockHeld, nil)
		}
		before := encode(issue)
		issue = applyIssuePatch(issue, patch)
		if encode(issue) == before {
			return unchanged{}
		}
		issue, err = finishIssueEdit(issue, action.Actor, now)
		if err != nil {
			return err
		}
		action.ObserveIssue(issue)
		return s.saveIssue(ctx, tx, &issue, false)
	}, action.Events...)
	return issue, err
}

// stateport.IssueLockRequest acquires or renews a lock; locks expire after five minutes.

func (s *Store) ChangeIssueLock(ctx context.Context, action stateport.Action, request stateport.IssueLockRequest) error {
	action.Events = slices.Clone(action.Events)
	if request.Owner == "" {
		return refuse(invalid, nil)
	}
	return s.change(ctx, mutation{action.Actor, "issue.lock", "issue", formatID(request.ID), request}, func(tx *sql.Tx) error {
		issue, err := readIssue(ctx, tx, request.ID)
		if err != nil {
			return err
		}
		now := s.options.Clock().UTC()
		if issue.Lock != nil && issue.Lock.Owner != request.Owner && issue.Lock.ExpiresAt.After(now) {
			return refuse(lockHeld, nil)
		}
		issue.Lock = &model.IssueLock{Owner: request.Owner, AcquiredAt: now, ExpiresAt: now.Add(5 * time.Minute)}
		action.ObserveIssue(issue)
		return s.saveIssue(ctx, tx, &issue, false)
	}, action.Events...)
}

func (s *Store) CloseIssue(ctx context.Context, action stateport.Action, request stateport.IssueClose) error {
	action.Events = slices.Clone(action.Events)
	return s.change(ctx, mutation{action.Actor, "issue.close", "issue", formatID(request.ID), map[string]string{"reason": action.Reason}}, func(tx *sql.Tx) error {
		issue, err := readIssue(ctx, tx, request.ID)
		if err != nil {
			return err
		}
		if issue.Revision != request.Revision || issue.CIWorkID != request.WorkID {
			return refuse(issueChanged, nil)
		}
		now := s.options.Clock().UTC()
		if issue.Lock != nil && issue.Lock.ExpiresAt.After(now) && issue.Lock.Owner != action.Actor {
			return refuse(lockHeld, nil)
		}
		if issue.State == model.IssueClosed {
			return unchanged{}
		}
		issue.State = model.IssueClosed
		issue.ClosedAt = &now
		issue.UpdatedAt = now
		issue.Revision++
		action.ObserveIssue(issue)
		return s.saveIssue(ctx, tx, &issue, false)
	}, action.Events...)
}

func requireIssueVersion(issue model.Issue, patch model.IssuePatch) error {
	if patch.ExpectedRevision != nil && *patch.ExpectedRevision != issue.Revision {
		return refuse(issueChanged, nil)
	}
	return nil
}

func applyIssuePatch(issue model.Issue, patch model.IssuePatch) model.Issue {
	if patch.Title != nil {
		issue.Title = *patch.Title
	}
	if patch.Body != nil {
		issue.Body = *patch.Body
	}
	if patch.WorkStatus != nil {
		issue.WorkStatus = *patch.WorkStatus
	}
	if patch.Upstream != nil {
		issue.Upstream = *patch.Upstream
	}
	return issue
}

// ReopenIssue is an explicit versioned manual action; CI projection owns CI state.
func (s *Store) ReopenIssue(ctx context.Context, action stateport.Action, request stateport.IssueClose) error {
	action.Events = slices.Clone(action.Events)
	return s.change(ctx, mutation{action.Actor, "issue.reopen", "issue", formatID(request.ID), nil}, func(tx *sql.Tx) error {
		issue, err := readIssue(ctx, tx, request.ID)
		if err != nil {
			return err
		}
		if issue.Kind != model.IssueManual {
			return refuse(transition, nil)
		}
		if issue.Revision != request.Revision {
			return refuse(issueChanged, nil)
		}
		if issue.State == model.IssueOpen {
			return unchanged{}
		}
		now := s.options.Clock().UTC()
		if issue.Lock != nil && issue.Lock.Owner != action.Actor && issue.Lock.ExpiresAt.After(now) {
			return refuse(lockHeld, nil)
		}
		issue.State = model.IssueOpen
		issue.ClosedAt = nil
		issue.UpdatedAt = now
		issue.Revision++
		action.ObserveIssue(issue)
		return s.saveIssue(ctx, tx, &issue, false)
	}, action.Events...)
}

// DeleteIssue removes only an unreferenced manual record under its current lock and revision.
func (s *Store) DeleteIssue(ctx context.Context, action stateport.Action, request stateport.IssueClose) error {
	action.Events = slices.Clone(action.Events)
	return s.change(ctx, mutation{action.Actor, "issue.delete", "issue", formatID(request.ID), nil}, func(tx *sql.Tx) error {
		issue, err := readIssue(ctx, tx, request.ID)
		if err != nil {
			return err
		}
		if err := action.CheckIssueDeletion(issue); err != nil {
			return err
		}
		if issue.Kind != model.IssueManual {
			return refuse(transition, nil)
		}
		if issue.Revision != request.Revision {
			return refuse(issueChanged, nil)
		}
		if issue.Lock != nil && issue.Lock.Owner != action.Actor && issue.Lock.ExpiresAt.After(s.options.Clock()) {
			return refuse(lockHeld, nil)
		}
		action.ObserveIssue(issue)
		if _, err := tx.ExecContext(ctx, `DELETE FROM issues WHERE id=?`, issue.ID); err != nil {
			return refuse(conflict, err)
		}
		s.noteIssueWrite(issue.ID)
		return nil
	}, action.Events...)
}

func finishIssueEdit(issue model.Issue, actor string, now time.Time) (model.Issue, error) {
	if issue.Title == "" {
		return issue, refuse(invalid, nil)
	}
	issue.Revision++
	issue.UpdatedAt = now
	if issue.Lock != nil && issue.Lock.Owner == actor {
		issue.Lock.ExpiresAt = now.Add(5 * time.Minute)
	}
	return issue, nil
}
