package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// Promote tag_after (#1524): one durable request per promotion, settled by
// the agent event recorder once the promotion is terminal.

// MaxPromotionTagMessage bounds an annotated tag message.
const MaxPromotionTagMessage = 4096

const promotionTagColumns = `promotion_id, actor, name, message, status, reason, object_sha, attempts, retry_after, requested_at, updated_at`

func scanPromotionTag(scan func(...any) error) (model.PromotionTag, error) {
	var value model.PromotionTag
	var retryAfter, requested, updated int64
	if err := scan(&value.PromotionID, &value.Actor, &value.Name, &value.Message, &value.Status, &value.Reason, &value.ObjectSHA, &value.Attempts, &retryAfter, &requested, &updated); err != nil {
		return model.PromotionTag{}, err
	}
	if retryAfter != 0 && value.Status == model.PromotionTagPending {
		at := fromUnixNano(retryAfter)
		value.NextRetryAt = &at
	}
	value.RequestedAt, value.UpdatedAt = fromUnixNano(requested), fromUnixNano(updated)
	return value, nil
}

// RequestPromotionTag records a tag_after request for an admitted promotion.
// A second request for the same promotion is a no-op.
func (s *Store) RequestPromotionTag(ctx context.Context, promotionID, actor, name, message string) error {
	if strings.TrimSpace(promotionID) == "" || strings.TrimSpace(actor) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(message) == "" || len(message) > MaxPromotionTagMessage {
		return fmt.Errorf("%w: promotion, actor, tag name and a message of at most %d bytes are required", ErrInvalid, MaxPromotionTagMessage)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin promotion tag request: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO promotion_tags(promotion_id, actor, name, message, status, requested_at, updated_at) VALUES(?,?,?,?,'pending',?,?)`,
		promotionID, actor, name, message, now, now)
	if err != nil {
		return fmt.Errorf("record promotion tag request: %w", err)
	}
	if inserted, _ := result.RowsAffected(); inserted == 1 {
		details, err := json.Marshal(map[string]any{"promotion_id": promotionID, "tag": name})
		if err != nil {
			return err
		}
		if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "promotion.tag_after.requested",
			ResourceType: "promotion", ResourceID: promotionID, Details: string(details)}, now); err != nil {
			return fmt.Errorf("audit promotion tag request: %w", err)
		}
	}
	return tx.Commit()
}

// PromotionTag returns the tag_after request of one promotion, or ErrNotFound.
func (s *Store) PromotionTag(ctx context.Context, promotionID string) (model.PromotionTag, error) {
	value, err := scanPromotionTag(s.reader().QueryRowContext(ctx, `SELECT `+promotionTagColumns+` FROM promotion_tags WHERE promotion_id = ?`, promotionID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return model.PromotionTag{}, fmt.Errorf("%w: promotion tag", ErrNotFound)
	}
	return value, err
}

// PendingPromotionTags lists the oldest pending requests.
func (s *Store) PendingPromotionTags(ctx context.Context, limit int) ([]model.PromotionTag, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.reader().QueryContext(ctx, `SELECT `+promotionTagColumns+` FROM promotion_tags WHERE status = 'pending' ORDER BY requested_at LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending promotion tags: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var values []model.PromotionTag
	for rows.Next() {
		value, err := scanPromotionTag(rows.Scan)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// RetryPromotionTag records one failed transient attempt of a pending
// request and when the next may run; the request stays pending.
func (s *Store) RetryPromotionTag(ctx context.Context, promotionID string, attempts int, retryAfter time.Time, reason string) (model.PromotionTag, error) {
	if len(reason) > maximumPromotionPruneText {
		reason = reason[:maximumPromotionPruneText]
	}
	now := unixNano(s.now())
	if _, err := s.db.ExecContext(ctx, `UPDATE promotion_tags SET attempts = ?, retry_after = ?, reason = ?, updated_at = ? WHERE promotion_id = ? AND status = 'pending'`,
		attempts, unixNano(retryAfter), reason, now, promotionID); err != nil {
		return model.PromotionTag{}, fmt.Errorf("record promotion tag retry: %w", err)
	}
	return s.PromotionTag(ctx, promotionID)
}

// FinishPromotionTag settles a pending request as pushed or failed, with the
// reason and the pushed tag object, and audits the decision in the same
// transaction. A settled request is never changed again.
func (s *Store) FinishPromotionTag(ctx context.Context, promotionID, status, reason, objectSHA string, attempts int) (model.PromotionTag, error) {
	if status != model.PromotionTagPushed && status != model.PromotionTagFailed {
		return model.PromotionTag{}, fmt.Errorf("%w: promotion tag status %q", ErrInvalid, status)
	}
	if len(reason) > maximumPromotionPruneText {
		reason = reason[:maximumPromotionPruneText]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.PromotionTag{}, fmt.Errorf("begin promotion tag: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanPromotionTag(tx.QueryRowContext(ctx, `SELECT `+promotionTagColumns+` FROM promotion_tags WHERE promotion_id = ?`, promotionID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return model.PromotionTag{}, fmt.Errorf("%w: promotion tag", ErrNotFound)
	}
	if err != nil {
		return model.PromotionTag{}, err
	}
	if current.Status != model.PromotionTagPending {
		return current, nil
	}
	now := unixNano(s.now())
	if _, err := tx.ExecContext(ctx, `UPDATE promotion_tags SET status = ?, reason = ?, object_sha = ?, attempts = ?, retry_after = 0, updated_at = ? WHERE promotion_id = ? AND status = 'pending'`,
		status, reason, objectSHA, attempts, now, promotionID); err != nil {
		return model.PromotionTag{}, fmt.Errorf("finish promotion tag: %w", err)
	}
	details, err := json.Marshal(map[string]any{"promotion_id": promotionID, "tag": current.Name, "status": status, "reason": reason, "object_sha": objectSHA, "attempts": attempts})
	if err != nil {
		return model.PromotionTag{}, err
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: current.Actor, Action: "promotion.tag_after." + status,
		ResourceType: "promotion", ResourceID: promotionID, Details: string(details)}, now); err != nil {
		return model.PromotionTag{}, fmt.Errorf("audit promotion tag: %w", err)
	}
	updated, err := scanPromotionTag(tx.QueryRowContext(ctx, `SELECT `+promotionTagColumns+` FROM promotion_tags WHERE promotion_id = ?`, promotionID).Scan)
	if err != nil {
		return model.PromotionTag{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.PromotionTag{}, fmt.Errorf("commit promotion tag: %w", err)
	}
	return updated, nil
}
