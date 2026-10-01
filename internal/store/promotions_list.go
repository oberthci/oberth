package store

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// ListPromotions reads the append-only admission sequence. It includes pending
// admissions without a run/result SHA, so a lost promote reply is recoverable.
func (s *Store) ListPromotions(ctx context.Context, filter model.PromotionListFilter) (model.PromotionPage, error) {
	if filter.RepoID < 0 || filter.Before < 0 || filter.Limit < 0 || filter.Limit > 200 ||
		(filter.SourceSHA != "" && !validOID(filter.SourceSHA)) || (filter.Status != "" && !filter.Status.Valid()) {
		return model.PromotionPage{}, fmt.Errorf("%w: promotion list filter", ErrInvalid)
	}
	limit := filter.Limit
	if limit == 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT sequence, id, repo_id, source_branch, source_sha, target_ref, previous_sha,
       result_sha, actor, status, run_id, error, created_at, updated_at
FROM promotions
WHERE (? = 0 OR repo_id = ?) AND (? = '' OR source_sha = ?)
  AND (? = '' OR status = ?) AND (? = 0 OR sequence < ?)
ORDER BY sequence DESC LIMIT ?`, filter.RepoID, filter.RepoID,
		filter.SourceSHA, filter.SourceSHA, filter.Status, filter.Status,
		filter.Before, filter.Before, limit+1)
	if err != nil {
		return model.PromotionPage{}, fmt.Errorf("list promotions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page := model.PromotionPage{Promotions: []model.Promotion{}}
	for rows.Next() {
		promotion, err := scanPromotion(rows)
		if err != nil {
			return model.PromotionPage{}, fmt.Errorf("scan promotion: %w", err)
		}
		if len(page.Promotions) == limit {
			page.NextBefore = page.Promotions[limit-1].Sequence
			break
		}
		page.Promotions = append(page.Promotions, promotion)
	}
	if err := rows.Err(); err != nil {
		return model.PromotionPage{}, fmt.Errorf("list promotions: %w", err)
	}
	return page, nil
}
