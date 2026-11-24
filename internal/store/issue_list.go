package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// Issues lists the issues of canonical repositories (nil scope: all), newest
// first; an upstream's repository-less issues follow its repositories.
func (s *Store) Issues(ctx context.Context, scope []string, filter model.IssueFilter) ([]model.Issue, error) {
	predicate, args := scopePredicate("repo", scope)
	if scope != nil {
		upstreams := []string{}
		for _, repo := range scope {
			upstreams = append(upstreams, strings.SplitN(repo, "/", 2)[0])
		}
		p, a := scopePredicate("upstream", upstreams)
		predicate = "(" + predicate + " OR (repo='' AND " + p + "))"
		args = append(args, a...)
	}
	page, err := s.issuePage(ctx, predicate, args, filter)
	return page.Issues, err
}

// ScopedIssues lists an upstream scope's issues with the page's totals.
func (s *Store) ScopedIssues(ctx context.Context, scope stateport.IssueScope, filter model.IssueFilter) (stateport.IssuePage, error) {
	predicate, args := scopedIssuePredicate(scope)
	return s.issuePage(ctx, predicate, args, filter)
}

// issueList is one list read: the issue set (scope, repository, kind and
// query) inside a read-only transaction, and the filter that pages it.
type issueList struct {
	tx     *sql.Tx
	set    string
	args   []any
	filter model.IssueFilter
}

// issuePage reads the page and its totals in one snapshot, so a page's
// position always agrees with its counts. Scope applies before anything else.
func (s *Store) issuePage(ctx context.Context, scope string, scopeArgs []any, f model.IssueFilter) (stateport.IssuePage, error) {
	page := stateport.IssuePage{Issues: []model.Issue{}}
	if err := validateIssueFilter(f); err != nil {
		return page, err
	}
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	list := issueList{tx: tx, filter: f}
	list.set, list.args = issueSet(scope, scopeArgs, f, s.options.Clock())
	if page.Issues, err = list.rows(ctx); err != nil {
		return page, err
	}
	return page, list.totals(ctx, &page)
}

// issueSet selects the scope's issues matching the filter's repository, kind
// and query, with each one's work status and stale observation: the rule of
// model.IssueWindows.Stale, compared at the database's millisecond precision.
func issueSet(scope string, args []any, f model.IssueFilter, now time.Time) (string, []any) {
	query := `SELECT id, value, state, json_extract(value,'$.work_status') AS work_status,` +
		` coalesce(state='open' AND julianday(json_extract(value,'$.updated_at'))<=julianday(CASE json_extract(value,'$.work_status') WHEN 'active' THEN ? ELSE ? END),0) AS stale` +
		` FROM issues WHERE ` + scope
	args = append([]any{staleCutoff(now, f.Windows.Active), staleCutoff(now, f.Windows.General)}, args...)
	if f.Repo != "" {
		query += ` AND repo=?`
		args = append(args, f.Repo)
	}
	if f.Kind != "" {
		query += ` AND json_extract(value,'$.kind')=?`
		args = append(args, string(f.Kind))
	}
	if f.Query != "" {
		query += ` AND instr(lower(json_extract(value,'$.title') || ' ' || json_extract(value,'$.body')),lower(?))>0`
		args = append(args, f.Query)
	}
	return query, args
}

// staleCutoff is the latest update time that is stale at now; a zero window has none.
func staleCutoff(now time.Time, window time.Duration) any {
	if window <= 0 {
		return nil
	}
	return now.Add(-window).UTC().Format(time.RFC3339Nano)
}

// selection is the filter's state, work status and stale choice over the set.
func (l issueList) selection() (string, []any) {
	where, args := "1", []any{}
	if l.filter.State != "" && l.filter.State != "all" {
		where += ` AND state=?`
		args = append(args, l.filter.State)
	}
	if l.filter.WorkStatus != "" {
		where += ` AND work_status=?`
		args = append(args, string(l.filter.WorkStatus))
	}
	if l.filter.Stale {
		where += ` AND stale`
	}
	return where, args
}

func (l issueList) rows(ctx context.Context) ([]model.Issue, error) {
	where, args := l.selection()
	order := "DESC"
	if l.filter.Before > 0 {
		where += ` AND id<?`
		args = append(args, l.filter.Before)
	}
	if l.filter.After > 0 {
		where += ` AND id>?`
		args = append(args, l.filter.After)
		order = "ASC"
	}
	query := `SELECT value, stale FROM (` + l.set + `) WHERE ` + where + ` ORDER BY id ` + order + ` LIMIT ?`
	rows, err := l.tx.QueryContext(ctx, query, slices.Concat(l.args, args, []any{l.filter.Limit})...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	out := []model.Issue{}
	for rows.Next() {
		var body string
		var stale bool
		var v model.Issue
		if err := rows.Scan(&body, &stale); err != nil {
			return nil, databaseError(err)
		}
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			return nil, refuse(corrupt, err)
		}
		v.Stale = stale
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(err)
	}
	if l.filter.After > 0 {
		slices.Reverse(out)
	}
	return out, nil
}

// totals is one aggregate pass over the set: the counts, the filter's total
// and how many of those are newer than the page. The set is materialized
// without bodies, so each issue's record is decoded once.
func (l issueList) totals(ctx context.Context, page *stateport.IssuePage) error {
	where, args := l.selection()
	query := `WITH listed AS MATERIALIZED (SELECT id, state, work_status, stale FROM (` + l.set + `))` +
		` SELECT count(*) FILTER (WHERE state='open'), count(*) FILTER (WHERE state='closed'),` +
		` count(*) FILTER (WHERE state='open' AND work_status='todo'), count(*) FILTER (WHERE state='open' AND work_status='active'),` +
		` count(*) FILTER (WHERE state='open' AND work_status='waiting'), count(*) FILTER (WHERE state='open' AND work_status='review'),` +
		` count(*) FILTER (WHERE stale), count(*) FILTER (WHERE ` + where + `), count(*) FILTER (WHERE ` + where + ` AND id>?)` +
		` FROM listed`
	c := &page.Counts
	err := l.tx.QueryRowContext(ctx, query, slices.Concat(l.args, args, args, []any{newerThan(page.Issues, l.filter)})...).
		Scan(&c.Open, &c.Closed, &c.Todo, &c.Active, &c.Waiting, &c.Review, &c.Stale, &page.Total, &page.Offset)
	return databaseError(err)
}

// newerThan is the ID above which an issue is newer than the page: its newest
// issue, or its cursor when the page is empty.
func newerThan(rows []model.Issue, f model.IssueFilter) int64 {
	switch {
	case len(rows) > 0:
		return rows[0].ID
	case f.Before > 0:
		return f.Before - 1
	default:
		return f.After
	}
}

func validateIssueFilter(filter model.IssueFilter) error {
	if filter.Limit < 1 || filter.Limit > 200 || !validIssuePaging(filter) {
		return refuse(invalid, nil)
	}
	if filter.WorkStatus != "" && !filter.WorkStatus.Valid() {
		return refuse(invalid, nil)
	}
	if filter.Kind != "" && !filter.Kind.Valid() {
		return refuse(invalid, nil)
	}
	if filter.State != "" && filter.State != "all" && filter.State != "open" && filter.State != "closed" {
		return refuse(invalid, nil)
	}
	return nil
}

// validIssuePaging allows one cursor at a time and no negative cursor or window.
func validIssuePaging(f model.IssueFilter) bool {
	return f.Before >= 0 && f.After >= 0 && (f.Before == 0 || f.After == 0) && f.Windows.General >= 0 && f.Windows.Active >= 0
}
