package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// Promotion rows are immutable once terminal, so a retry (#1097) is a new
// promotion of the same (repository, source SHA, target), merged afresh onto
// the target's current head and re-tested. Its lineage lives in the audit
// chain: one promotion.retry action per retry, recorded on the new promotion
// with the promotion it re-runs, in the same transaction as its admission.

type promotionRetryAdmission struct {
	retryOf     string
	mode        string
	autoAttempt int
}

func validatePromotionRetrySpec(spec model.PromotionSpec) error {
	switch {
	case spec.RetryOf == "" && spec.RetryMode == "":
		return nil
	case strings.TrimSpace(spec.RetryOf) == "":
		return fmt.Errorf("%w: a promotion retry mode needs the promotion it retries", ErrInvalid)
	case spec.RetryMode != model.PromotionRetryManual && spec.RetryMode != model.PromotionRetryAuto:
		return fmt.Errorf("%w: promotion retry mode must be %q or %q", ErrInvalid, model.PromotionRetryManual, model.PromotionRetryAuto)
	}
	return nil
}

// checkPromotionRetryTx admits a retry of spec.RetryOf only while that
// promotion is failed or interrupted, it was never retried before, and no
// promotion of the same tuple is pending or delivered. The checks and the
// insert share one transaction, so two concurrent retries cannot both pass.
func checkPromotionRetryTx(ctx context.Context, tx *sql.Tx, spec model.PromotionSpec) (promotionRetryAdmission, error) {
	original, err := promotionTx(ctx, tx, spec.RetryOf)
	if err != nil {
		return promotionRetryAdmission{}, err
	}
	if original.RepoID != spec.RepoID || !strings.EqualFold(original.SourceSHA, spec.SourceSHA) || original.TargetRef != spec.TargetRef {
		return promotionRetryAdmission{}, fmt.Errorf("%w: a promotion retry must keep the repository, source SHA and target of %s", ErrInvalid, original.ID)
	}
	if original.Status != model.PromotionFailed && original.Status != model.PromotionInterrupted {
		return promotionRetryAdmission{}, fmt.Errorf("%w: promotion %s is %s; only a failed or interrupted promotion can be retried", ErrInvalidState, original.ID, original.Status)
	}
	var blockerID, blockerStatus string
	err = tx.QueryRowContext(ctx, `
SELECT id, status FROM promotions
WHERE repo_id = ? AND lower(source_sha) = lower(?) AND target_ref = ? AND status IN ('pending', 'passed')
ORDER BY sequence DESC LIMIT 1`, original.RepoID, original.SourceSHA, original.TargetRef).Scan(&blockerID, &blockerStatus)
	switch {
	case err == nil && blockerStatus == string(model.PromotionPassed):
		return promotionRetryAdmission{}, fmt.Errorf("%w: promotion %s already delivered %s to %s", ErrInvalidState, blockerID, original.SourceSHA, original.TargetRef)
	case err == nil:
		return promotionRetryAdmission{}, fmt.Errorf("%w: promotion %s of %s to %s is still pending", ErrInvalidState, blockerID, original.SourceSHA, original.TargetRef)
	case !errors.Is(err, sql.ErrNoRows):
		return promotionRetryAdmission{}, fmt.Errorf("check promotions of the retried tuple: %w", err)
	}
	successor, err := promotionSuccessor(ctx, tx, original.ID)
	if err != nil {
		return promotionRetryAdmission{}, err
	}
	if successor != "" {
		return promotionRetryAdmission{}, fmt.Errorf("%w: promotion %s was already retried as %s", ErrInvalidState, original.ID, successor)
	}
	admission := promotionRetryAdmission{retryOf: original.ID, mode: spec.RetryMode}
	if spec.RetryMode == model.PromotionRetryAuto {
		previous, err := promotionRetryRecord(ctx, tx, original.ID)
		if err != nil {
			return promotionRetryAdmission{}, err
		}
		if previous.Mode == model.PromotionRetryAuto {
			admission.autoAttempt = previous.AutoAttempt
		}
		admission.autoAttempt++
	}
	return admission, nil
}

func (s *Store) appendPromotionRetryAudit(ctx context.Context, tx *sql.Tx, promotion model.Promotion, admission promotionRetryAdmission, now int64) error {
	details, err := json.Marshal(map[string]string{
		"retry_of":     admission.retryOf,
		"mode":         admission.mode,
		"auto_attempt": strconv.Itoa(admission.autoAttempt),
		"source_sha":   promotion.SourceSHA,
		"target_ref":   promotion.TargetRef,
	})
	if err != nil {
		return fmt.Errorf("encode promotion retry audit: %w", err)
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: promotion.Actor, Action: "promotion.retry", ResourceType: "promotion",
		ResourceID: promotion.ID, Details: string(details),
	}, now); err != nil {
		return fmt.Errorf("append promotion retry audit: %w", err)
	}
	return nil
}

// checkPromotionGrantReservableTx lets a promotion hold grantID only while
// the grant matches its exact tuple, is unrevoked, unexpired and unconsumed,
// and no other pending or delivered promotion holds it. Holding is the
// promotion's own grant_id; consumption follows only its delivery.
func checkPromotionGrantReservableTx(ctx context.Context, tx *sql.Tx, spec model.PromotionSpec, now int64) error {
	grant, err := scanPromotionGrant(tx.QueryRowContext(ctx, `
SELECT id, repo_id, source_sha, target_ref, issuer, issuer_fingerprint, note,
       created_at, expires_at, revoked_by, revoked_at, consumed_by, consumed_at
FROM promotion_grants WHERE id = ?`, spec.GrantID))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: promotion grant %s", ErrNotFound, spec.GrantID)
	}
	if err != nil {
		return fmt.Errorf("load promotion grant: %w", err)
	}
	switch {
	case grant.RepoID != spec.RepoID || !strings.EqualFold(grant.SourceSHA, spec.SourceSHA) || grant.TargetRef != spec.TargetRef:
		return fmt.Errorf("%w: promotion grant %s does not name this repository, SHA and target", ErrInvalidState, grant.ID)
	case grant.RevokedAt != nil:
		return fmt.Errorf("%w: promotion grant %s is revoked", ErrInvalidState, grant.ID)
	case grant.ConsumedAt != nil:
		return fmt.Errorf("%w: promotion grant %s was consumed by promotion %s", ErrInvalidState, grant.ID, grant.ConsumedBy)
	case unixNano(grant.ExpiresAt) <= now:
		return fmt.Errorf("%w: promotion grant %s has expired", ErrInvalidState, grant.ID)
	}
	var holder string
	err = tx.QueryRowContext(ctx, `
SELECT id FROM promotions WHERE grant_id = ? AND status IN ('pending', 'passed')
ORDER BY sequence DESC LIMIT 1`, grant.ID).Scan(&holder)
	if err == nil {
		return fmt.Errorf("%w: promotion grant %s is held by promotion %s", ErrInvalidState, grant.ID, holder)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check promotion grant holder: %w", err)
	}
	return nil
}

func (s *Store) appendPromotionGrantReserveAudit(ctx context.Context, tx *sql.Tx, promotion model.Promotion, retryOf string, now int64) error {
	details, err := json.Marshal(map[string]string{"promotion_id": promotion.ID, "retry_of": retryOf})
	if err != nil {
		return fmt.Errorf("encode promotion grant reservation audit: %w", err)
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: promotion.Actor, Action: "promotion_grant.reserve", ResourceType: "promotion_grant",
		ResourceID: promotion.GrantID, Details: string(details),
	}, now); err != nil {
		return fmt.Errorf("audit promotion grant reservation: %w", err)
	}
	return nil
}

// consumeDeliveredPromotionGrantTx consumes the grant a promotion holds, in
// the transaction that records its delivered publication. A grant consumed at
// admission by an older server is already consumed and is left unchanged.
func (s *Store) consumeDeliveredPromotionGrantTx(ctx context.Context, tx *sql.Tx, promotion model.Promotion, publicationID string, now int64) error {
	if promotion.GrantID == "" {
		return nil
	}
	result, err := tx.ExecContext(ctx, `
UPDATE promotion_grants SET consumed_by = ?, consumed_at = ?
WHERE id = ? AND consumed_at IS NULL`, promotion.ID, now, promotion.GrantID)
	if err != nil {
		return fmt.Errorf("consume delivered promotion grant: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("consume delivered promotion grant rows: %w", err)
	}
	if changed == 0 {
		return nil
	}
	details, err := json.Marshal(map[string]string{"promotion_id": promotion.ID, "publication_id": publicationID})
	if err != nil {
		return fmt.Errorf("encode promotion grant consumption audit: %w", err)
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: promotion.Actor, Action: "promotion_grant.consume", ResourceType: "promotion_grant",
		ResourceID: promotion.GrantID, Details: string(details),
	}, now); err != nil {
		return fmt.Errorf("audit promotion grant consumption: %w", err)
	}
	return nil
}

type auditQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// promotionRetryRecord reads the promotion.retry action recorded when id was
// admitted as a retry; a fresh promotion has none and yields a zero lineage.
func promotionRetryRecord(ctx context.Context, queryer auditQueryer, id string) (model.PromotionLineage, error) {
	var details string
	err := queryer.QueryRowContext(ctx, `
SELECT details FROM audit_actions
WHERE action = 'promotion.retry' AND resource_type = 'promotion' AND resource_id = ?
ORDER BY id DESC LIMIT 1`, id).Scan(&details)
	if errors.Is(err, sql.ErrNoRows) {
		return model.PromotionLineage{}, nil
	}
	if err != nil {
		return model.PromotionLineage{}, fmt.Errorf("read promotion retry lineage: %w", err)
	}
	var fields map[string]string
	if err := json.Unmarshal([]byte(details), &fields); err != nil {
		return model.PromotionLineage{}, fmt.Errorf("decode promotion retry lineage: %w", err)
	}
	attempt, _ := strconv.Atoi(fields["auto_attempt"])
	return model.PromotionLineage{RetryOf: fields["retry_of"], Mode: fields["mode"], AutoAttempt: attempt}, nil
}

func promotionSuccessor(ctx context.Context, queryer auditQueryer, id string) (string, error) {
	var successor string
	err := queryer.QueryRowContext(ctx, `
SELECT resource_id FROM audit_actions
WHERE action = 'promotion.retry' AND resource_type = 'promotion'
  AND json_extract(details, '$.retry_of') = ?
ORDER BY id LIMIT 1`, id).Scan(&successor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read promotion retry successor: %w", err)
	}
	return successor, nil
}

// PromotionLineage reports which promotion id retries and which promotion
// retried it, from the audit chain.
func (s *Store) PromotionLineage(ctx context.Context, id string) (model.PromotionLineage, error) {
	if strings.TrimSpace(id) == "" {
		return model.PromotionLineage{}, fmt.Errorf("%w: promotion ID is required", ErrInvalid)
	}
	lineage, err := promotionRetryRecord(ctx, s.reader(), id)
	if err != nil {
		return model.PromotionLineage{}, err
	}
	lineage.RetriedAs, err = promotionSuccessor(ctx, s.reader(), id)
	if err != nil {
		return model.PromotionLineage{}, err
	}
	return lineage, nil
}

// UplinksByIdentity returns every current uplink with identity, with its
// admin flag and scope, so the server's own promotion retry can re-check the
// original actor's authority as it is now (#1097). A removed uplink yields
// none.
func (s *Store) UplinksByIdentity(ctx context.Context, identity string) ([]model.Uplink, error) {
	if strings.TrimSpace(identity) == "" {
		return nil, fmt.Errorf("%w: uplink identity is required", ErrInvalid)
	}
	fingerprints, err := s.uplinkFingerprintsByIdentity(ctx, identity)
	if err != nil {
		return nil, err
	}
	uplinks := make([]model.Uplink, 0, len(fingerprints))
	for _, fingerprint := range fingerprints {
		uplink, err := s.UplinkByFingerprint(ctx, fingerprint)
		if err != nil {
			return nil, err
		}
		uplinks = append(uplinks, uplink)
	}
	return uplinks, nil
}

func (s *Store) uplinkFingerprintsByIdentity(ctx context.Context, identity string) ([]string, error) {
	rows, err := s.reader().QueryContext(ctx, `SELECT fingerprint FROM uplinks WHERE identity = ? ORDER BY id`, identity)
	if err != nil {
		return nil, fmt.Errorf("list uplinks by identity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var fingerprints []string
	for rows.Next() {
		var fingerprint string
		if err := rows.Scan(&fingerprint); err != nil {
			return nil, fmt.Errorf("scan uplink fingerprint: %w", err)
		}
		fingerprints = append(fingerprints, fingerprint)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list uplinks by identity: %w", err)
	}
	return fingerprints, nil
}
