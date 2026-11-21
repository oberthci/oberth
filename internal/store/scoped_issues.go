package store

import (
	"context"
	"encoding/json"

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
