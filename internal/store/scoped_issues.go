package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func scopedIssuePredicate(scope stateport.IssueScope) (string, []any) {
	p, a := scopePredicate("upstream", scope.Upstreams)
	p = "(" + p + " AND upstream NOT IN ('','global'))"
	if scope.Global {
		p += " OR upstream IN ('','global')"
	}
	return "(" + p + ")", a
}

// ScopedIssue filters ownership before decoding the requested body.
func (s *Store) ScopedIssue(ctx context.Context, scope stateport.IssueScope, id int64) (model.Issue, error) {
	p, a := scopedIssuePredicate(scope)
	var body string
	err := s.readers.QueryRowContext(ctx, `SELECT value FROM issues WHERE id=? AND `+p, append([]any{id}, a...)...).Scan(&body)
	var v model.Issue
	if err != nil {
		return v, databaseError(err)
	}
	if err = json.Unmarshal([]byte(body), &v); err != nil {
		return v, refuse(corrupt, err)
	}
	return v, nil
}
func (s *Store) ScopedIssues(ctx context.Context, scope stateport.IssueScope, f model.IssueFilter) ([]model.Issue, error) {
	if err := validateIssueFilter(f); err != nil {
		return nil, err
	}
	query, args := issueQuery(nil, f)
	predicate, scoped := scopedIssuePredicate(scope)
	query = strings.Replace(query, "WHERE 1", "WHERE ("+predicate+")", 1)
	args = append(scoped, args...)
	rows, err := s.readers.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	return s.scopedIssueRows(rows, f)
}
func (s *Store) scopedIssueRows(rows *sql.Rows, f model.IssueFilter) ([]model.Issue, error) {
	out := []model.Issue{}
	for rows.Next() {
		var v model.Issue
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, databaseError(err)
		}
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, refuse(corrupt, err)
		}
		v.Stale = issueStale(v, f, s.options.Clock())
		out = append(out, v)
	}
	return out, databaseError(rows.Err())
}
