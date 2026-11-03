package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// ResolvePromotionID resolves public selectors; Promotion and mutation lookups
// remain exact so a pinned operation cannot switch to another record.
func (s *Store) ResolvePromotionID(ctx context.Context, id string) (model.Promotion, error) {
	return s.ResolvePromotionIDIn(ctx, id, nil)
}

// ResolvePromotionIDIn restricts visibility before testing uniqueness.
func (s *Store) ResolvePromotionIDIn(ctx context.Context, id string, repoIDs []int64) (model.Promotion, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) < 8 || len(id) > 32 || !isHexPrefix(id) {
		return model.Promotion{}, fmt.Errorf("%w: promotion ID must be 8 to 32 hexadecimal characters", ErrInvalid)
	}
	restriction, args := repoRestriction("repo_id", repoIDs)
	rows, err := s.reader().QueryContext(ctx, `SELECT id FROM promotions WHERE id >= ? AND id < ?`+restriction+` ORDER BY id`, // #nosec G202 -- fixed column and placeholders from repoRestriction.
		append([]any{id, id + "g"}, args...)...)
	if err != nil {
		return model.Promotion{}, fmt.Errorf("resolve promotion ID: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var matches []string
	for rows.Next() {
		var match string
		if err := rows.Scan(&match); err != nil {
			return model.Promotion{}, err
		}
		matches = append(matches, match)
	}
	if err := rows.Err(); err != nil {
		return model.Promotion{}, err
	}
	if err := rows.Close(); err != nil {
		return model.Promotion{}, err
	}
	switch len(matches) {
	case 0:
		return model.Promotion{}, fmt.Errorf("%w: promotion ID %q", ErrNotFound, id)
	case 1:
		return s.Promotion(ctx, matches[0])
	default:
		return model.Promotion{}, fmt.Errorf("%w: promotion ID prefix %q matches: %s; use the full promotion ID", ErrAmbiguous, id, strings.Join(matches, ", "))
	}
}
