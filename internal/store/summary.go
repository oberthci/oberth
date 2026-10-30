package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ContentSummary counts what a database holds, for `oberth backup` and
// `oberth restore` to say what an archive carries and what a restore
// replaces. It carries names and counts only: no tokens, digests or bodies.
type ContentSummary struct {
	Runs       int
	LastRunAt  time.Time
	Issues     int
	OpenIssues int
	Promotions int
	// Repositories are "upstream/name"; Upstreams are names.
	Repositories []string
	Upstreams    []string
	// Uplinks are the identities that can authenticate, admin ones marked
	// " (admin)".
	Uplinks []string
}

// Summary reads the content summary.
func (s *Store) Summary(ctx context.Context) (ContentSummary, error) {
	var summary ContentSummary
	var last sql.NullInt64
	if err := s.reader().QueryRowContext(ctx, `SELECT count(*), max(created_at) FROM runs`).Scan(&summary.Runs, &last); err != nil {
		return summary, fmt.Errorf("count runs: %w", err)
	}
	if last.Valid {
		summary.LastRunAt = fromUnixNano(last.Int64)
	}
	if err := s.reader().QueryRowContext(ctx, `SELECT count(*), count(CASE WHEN state = 'open' THEN 1 END) FROM issues`).
		Scan(&summary.Issues, &summary.OpenIssues); err != nil {
		return summary, fmt.Errorf("count issues: %w", err)
	}
	if err := s.reader().QueryRowContext(ctx, `SELECT count(*) FROM promotions`).Scan(&summary.Promotions); err != nil {
		return summary, fmt.Errorf("count promotions: %w", err)
	}
	var err error
	if summary.Repositories, err = s.summaryNames(ctx, `
SELECT u.name || '/' || r.name FROM repositories r JOIN upstreams u ON u.id = r.upstream_id ORDER BY 1`); err != nil {
		return summary, fmt.Errorf("list repositories: %w", err)
	}
	if summary.Upstreams, err = s.summaryNames(ctx, `SELECT name FROM upstreams ORDER BY name`); err != nil {
		return summary, fmt.Errorf("list upstreams: %w", err)
	}
	if summary.Uplinks, err = s.summaryNames(ctx, `
SELECT identity || CASE WHEN admin THEN ' (admin)' ELSE '' END FROM uplinks ORDER BY identity`); err != nil {
		return summary, fmt.Errorf("list uplinks: %w", err)
	}
	return summary, nil
}

func (s *Store) summaryNames(ctx context.Context, query string) ([]string, error) {
	rows, err := s.reader().QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}
