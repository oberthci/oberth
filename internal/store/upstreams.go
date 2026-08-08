package store

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

func (s *Store) UpstreamByName(ctx context.Context, name string) (model.Upstream, error) {
	return scanUpstream(s.db.QueryRowContext(ctx, `
SELECT id, name, kind, base_url, created_at, updated_at FROM upstreams WHERE name = ?`, name))
}

func (s *Store) ListUpstreams(ctx context.Context) ([]model.Upstream, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, kind, base_url, created_at, updated_at FROM upstreams ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list upstreams: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var values []model.Upstream
	for rows.Next() {
		value, err := scanUpstream(rows)
		if err != nil {
			return nil, fmt.Errorf("scan upstream: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list upstreams: %w", err)
	}
	return values, nil
}

func scanUpstream(row rowScanner) (model.Upstream, error) {
	var value model.Upstream
	var created, updated int64
	if err := row.Scan(&value.ID, &value.Name, &value.Kind, &value.BaseURL, &created, &updated); err != nil {
		return model.Upstream{}, translateNotFound("upstream", err)
	}
	value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return value, nil
}
