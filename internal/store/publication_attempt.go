package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
)

// UpdatePublicationAttempt reserves a push before external I/O or records its
// backoff afterwards. Expected attempts makes lost database replies idempotent.
// Progress and the owner message share the same transaction and audit intent.
func (s *Store) UpdatePublicationAttempt(ctx context.Context, id string, expected, attempts int, retryAfter time.Time, message string) (model.Publication, error) {
	if attempts < 1 || attempts > pushretry.MaxAttempts || (attempts != expected && attempts != expected+1) || retryAfter.IsZero() {
		return model.Publication{}, fmt.Errorf("%w: invalid publication attempt", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Publication{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanPublication(tx.QueryRowContext(ctx, `SELECT `+publicationColumns+` FROM publications WHERE id=?`, id))
	if err != nil {
		return current, translateNotFound("publication", err)
	}
	if current.Status != model.PublicationPending {
		return current, fmt.Errorf("%w: publication is terminal", ErrInvalidState)
	}
	if attempts > expected && current.PromotionID != "" {
		promotion, err := promotionTx(ctx, tx, current.PromotionID)
		if err != nil {
			return model.Publication{}, err
		}
		if err := checkPromotionDeliveryGrantTx(ctx, tx, promotion, unixNano(s.now())); err != nil {
			return model.Publication{}, err
		}
	}
	if current.Attempts == attempts && current.RetryAfter.Equal(retryAfter) && current.Error == message {
		return current, nil
	}
	if current.Attempts != expected {
		return current, fmt.Errorf("%w: publication attempt changed", ErrInvalidState)
	}
	now := unixNano(s.now())
	value, err := scanPublication(tx.QueryRowContext(ctx, `UPDATE publications SET attempts=?,retry_after=?,error=?,updated_at=? WHERE id=? AND status='pending' AND attempts=? RETURNING `+publicationColumns, attempts, unixNano(retryAfter), message, now, id, expected))
	if err != nil {
		return value, err
	}
	if value.RunID != "" {
		runMessage, messageErr := publicationRunMessage(ctx, tx, value.RunID, message)
		if messageErr != nil {
			return value, messageErr
		}
		if _, err = tx.ExecContext(ctx, `UPDATE runs SET error=?,updated_at=? WHERE id=? AND status='running' AND phase='publishing'`, runMessage, now, value.RunID); err != nil {
			return value, err
		}
	}
	if value.PromotionID != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE promotions SET error=?,updated_at=? WHERE id=? AND status='pending'`, message, now, value.PromotionID); err != nil {
			return value, err
		}
	}
	// Use the canonical pending action: audit verification also binds the retry
	// budget and deadline, while retaining legacy zero-attempt intent bytes.
	if err = s.appendPublicationAudit(ctx, tx, value, "publication.pending", ""); err != nil {
		return value, err
	}
	if err = tx.Commit(); err != nil {
		return value, err
	}
	return value, nil
}

// FailedPromotionPublication supplies the exact failed push intent for a
// promotion retry. It cannot manufacture test success from a failed CI run.
func (s *Store) FailedPromotionPublication(ctx context.Context, id string) (model.Publication, error) {
	value, err := scanPublication(s.reader().QueryRowContext(ctx, `SELECT `+publicationColumns+` FROM publications WHERE promotion_id=? AND status='failed' ORDER BY sequence DESC LIMIT 1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return value, fmt.Errorf("%w: failed promotion publication", ErrNotFound)
	}
	return value, err
}
