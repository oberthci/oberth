package store

import (
	"context"
	"fmt"
	"strings"
)

// RunWorkspaceCleanupEligible reports whether the durable run owner can no
// longer use its workspace. Pending publications and incomplete cancellation
// obligations keep ownership even when the run projection is terminal.
func (s *Store) RunWorkspaceCleanupEligible(ctx context.Context, id string) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, fmt.Errorf("%w: run workspace owner is required", ErrInvalid)
	}
	var eligible bool
	if err := s.db.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1 FROM runs
    WHERE id = ?
      AND status IN ('passed', 'failed', 'interrupted')
      AND NOT EXISTS (
          SELECT 1 FROM publications
          WHERE publications.run_id = runs.id AND publications.status = 'pending'
      )
      AND NOT EXISTS (
          SELECT 1 FROM run_cancellations
          WHERE run_cancellations.run_id = runs.id AND run_cancellations.completed_at IS NULL
      )
)`, id).Scan(&eligible); err != nil {
		return false, fmt.Errorf("check run workspace owner %s: %w", id, err)
	}
	return eligible, nil
}

// PromotionWorkspaceCleanupEligible reports whether promotion state is
// terminal and no pending publication still owns its exact result.
func (s *Store) PromotionWorkspaceCleanupEligible(ctx context.Context, id string) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, fmt.Errorf("%w: promotion workspace owner is required", ErrInvalid)
	}
	var eligible bool
	if err := s.db.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1 FROM promotions
    WHERE id = ?
      AND status IN ('passed', 'failed', 'interrupted')
      AND NOT EXISTS (
          SELECT 1 FROM publications
          WHERE publications.promotion_id = promotions.id AND publications.status = 'pending'
      )
)`, id).Scan(&eligible); err != nil {
		return false, fmt.Errorf("check promotion workspace owner %s: %w", id, err)
	}
	return eligible, nil
}

// RunWorkspaceCleanupCandidates pages only durable owners that satisfy the
// same exact cleanup predicate used immediately before removal. The cursor is
// the last owner ID returned by the previous page.
func (s *Store) RunWorkspaceCleanupCandidates(ctx context.Context, afterID string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id FROM runs
WHERE id > ?
  AND status IN ('passed', 'failed', 'interrupted')
  AND NOT EXISTS (
      SELECT 1 FROM publications
      WHERE publications.run_id = runs.id AND publications.status = 'pending'
  )
  AND NOT EXISTS (
      SELECT 1 FROM run_cancellations
      WHERE run_cancellations.run_id = runs.id AND run_cancellations.completed_at IS NULL
  )
ORDER BY id
LIMIT ?`, afterID, pageLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("list run workspace cleanup candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	values, err := scanWorkspaceOwnerIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("list run workspace cleanup candidates: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list run workspace cleanup candidates: %w", err)
	}
	return values, nil
}

// PromotionWorkspaceCleanupCandidates is the promotion counterpart to
// RunWorkspaceCleanupCandidates.
func (s *Store) PromotionWorkspaceCleanupCandidates(ctx context.Context, afterID string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id FROM promotions
WHERE id > ?
  AND status IN ('passed', 'failed', 'interrupted')
  AND NOT EXISTS (
      SELECT 1 FROM publications
      WHERE publications.promotion_id = promotions.id AND publications.status = 'pending'
  )
ORDER BY id
LIMIT ?`, afterID, pageLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("list promotion workspace cleanup candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	values, err := scanWorkspaceOwnerIDs(rows)
	if err != nil {
		return nil, fmt.Errorf("list promotion workspace cleanup candidates: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list promotion workspace cleanup candidates: %w", err)
	}
	return values, nil
}

type workspaceOwnerRows interface {
	Next() bool
	Scan(...any) error
}

func scanWorkspaceOwnerIDs(rows workspaceOwnerRows) ([]string, error) {
	var values []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan workspace owner: %w", err)
		}
		values = append(values, id)
	}
	return values, nil
}
