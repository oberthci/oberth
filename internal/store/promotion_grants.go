package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// AppendPromotionGrant creates a new promotion grant with an audited record.
func (s *Store) AppendPromotionGrant(ctx context.Context, spec model.PromotionGrantSpec) (model.PromotionGrant, error) {
	if spec.RepoID <= 0 || !validOID(spec.SourceSHA) || strings.TrimSpace(spec.TargetRef) == "" ||
		strings.TrimSpace(spec.Issuer) == "" || strings.TrimSpace(spec.IssuerFingerprint) == "" {
		return model.PromotionGrant{}, fmt.Errorf("%w: promotion grant fields are invalid", ErrInvalid)
	}
	if !spec.ExpiresAt.After(s.now()) {
		return model.PromotionGrant{}, fmt.Errorf("%w: promotion grant expiry must be in the future", ErrInvalid)
	}
	id, err := randomID()
	if err != nil {
		return model.PromotionGrant{}, fmt.Errorf("generate promotion grant id: %w", err)
	}
	now := unixNano(s.now())
	expiresAt := unixNano(spec.ExpiresAt)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.PromotionGrant{}, fmt.Errorf("begin append promotion grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	value, err := scanPromotionGrant(tx.QueryRowContext(ctx, `
INSERT INTO promotion_grants(id, repo_id, source_sha, target_ref, issuer, issuer_fingerprint, note, created_at, expires_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, repo_id, source_sha, target_ref, issuer, issuer_fingerprint, note,
          created_at, expires_at, revoked_by, revoked_at, consumed_by, consumed_at`,
		id, spec.RepoID, strings.ToLower(spec.SourceSHA), spec.TargetRef,
		spec.Issuer, spec.IssuerFingerprint, spec.Note, now, expiresAt))
	if err != nil {
		return model.PromotionGrant{}, fmt.Errorf("insert promotion grant: %w", err)
	}
	details, jsonErr := json.Marshal(map[string]string{
		"repo_id":    fmt.Sprintf("%d", spec.RepoID),
		"source_sha": value.SourceSHA,
		"target_ref": value.TargetRef,
		"expires_at": value.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"),
	})
	if jsonErr != nil {
		return model.PromotionGrant{}, fmt.Errorf("encode promotion grant audit: %w", jsonErr)
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: spec.Issuer, Action: "promotion_grant.create", ResourceType: "promotion_grant",
		ResourceID: value.ID, Details: string(details),
	}, now); err != nil {
		return model.PromotionGrant{}, fmt.Errorf("audit promotion grant creation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.PromotionGrant{}, fmt.Errorf("commit promotion grant: %w", err)
	}
	return value, nil
}

// RevokePromotionGrant marks a grant as revoked. The grant must be active
// (neither already revoked, consumed, nor expired).
func (s *Store) RevokePromotionGrant(ctx context.Context, id, actor string) (model.PromotionGrant, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(actor) == "" {
		return model.PromotionGrant{}, fmt.Errorf("%w: grant ID and actor are required", ErrInvalid)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.PromotionGrant{}, fmt.Errorf("begin revoke promotion grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Load the grant first to produce a specific error.
	current, err := scanPromotionGrant(tx.QueryRowContext(ctx, `
SELECT id, repo_id, source_sha, target_ref, issuer, issuer_fingerprint, note,
       created_at, expires_at, revoked_by, revoked_at, consumed_by, consumed_at
FROM promotion_grants WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.PromotionGrant{}, fmt.Errorf("%w: promotion grant", ErrNotFound)
	}
	if err != nil {
		return model.PromotionGrant{}, fmt.Errorf("load promotion grant: %w", err)
	}
	if current.RevokedAt != nil {
		return model.PromotionGrant{}, fmt.Errorf("%w: promotion grant is already revoked", ErrInvalidState)
	}
	if current.ConsumedAt != nil {
		return model.PromotionGrant{}, fmt.Errorf("%w: promotion grant is already consumed", ErrInvalidState)
	}

	value, err := scanPromotionGrant(tx.QueryRowContext(ctx, `
UPDATE promotion_grants SET revoked_by = ?, revoked_at = ?
WHERE id = ? AND revoked_at IS NULL AND consumed_at IS NULL
RETURNING id, repo_id, source_sha, target_ref, issuer, issuer_fingerprint, note,
          created_at, expires_at, revoked_by, revoked_at, consumed_by, consumed_at`, actor, now, id))
	if err != nil {
		return model.PromotionGrant{}, fmt.Errorf("revoke promotion grant: %w", err)
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: "promotion_grant.revoke", ResourceType: "promotion_grant",
		ResourceID: value.ID, Details: "{}",
	}, now); err != nil {
		return model.PromotionGrant{}, fmt.Errorf("audit promotion grant revocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.PromotionGrant{}, fmt.Errorf("commit revoke promotion grant: %w", err)
	}
	return value, nil
}

// MatchingPromotionGrant finds an active (unexpired, unrevoked, unconsumed)
// grant for the exact (repo, sha, target) tuple that no pending promotion
// holds. A grant whose promotion failed before delivery is active again
// (#1097). Returns ErrNotFound when no matching active grant exists, and
// ErrInvalidState naming the holder when every matching grant is held.
func (s *Store) MatchingPromotionGrant(ctx context.Context, repoID int64, sha, target string) (model.PromotionGrant, error) {
	if repoID <= 0 || !validOID(sha) || strings.TrimSpace(target) == "" {
		return model.PromotionGrant{}, fmt.Errorf("%w: matching grant fields are required", ErrInvalid)
	}
	now := unixNano(s.now())
	value, err := scanPromotionGrant(s.reader().QueryRowContext(ctx, `
SELECT id, repo_id, source_sha, target_ref, issuer, issuer_fingerprint, note,
       created_at, expires_at, revoked_by, revoked_at, consumed_by, consumed_at
FROM promotion_grants
WHERE repo_id = ? AND source_sha = ? AND target_ref = ?
  AND revoked_at IS NULL AND consumed_at IS NULL AND expires_at > ?
  AND NOT EXISTS (
      SELECT 1 FROM promotions
      WHERE ((promotions.train_json='' AND promotions.grant_id = promotion_grants.id) OR EXISTS(SELECT 1 FROM promotion_train_grants g WHERE g.promotion_id=promotions.id AND g.grant_id=promotion_grants.id AND g.included=1)) AND promotions.status IN ('pending', 'passed')
  )
ORDER BY created_at LIMIT 1`, repoID, strings.ToLower(sha), target, now))
	if errors.Is(err, sql.ErrNoRows) {
		var holder string
		holderErr := s.reader().QueryRowContext(ctx, `
SELECT promotions.id FROM promotion_grants
JOIN promotions ON ((promotions.train_json='' AND promotions.grant_id = promotion_grants.id) OR EXISTS(SELECT 1 FROM promotion_train_grants g WHERE g.promotion_id=promotions.id AND g.grant_id=promotion_grants.id AND g.included=1)) AND promotions.status = 'pending'
WHERE promotion_grants.repo_id = ? AND promotion_grants.source_sha = ? AND promotion_grants.target_ref = ?
  AND promotion_grants.revoked_at IS NULL AND promotion_grants.consumed_at IS NULL AND promotion_grants.expires_at > ?
LIMIT 1`, repoID, strings.ToLower(sha), target, now).Scan(&holder)
		if holderErr == nil {
			return model.PromotionGrant{}, fmt.Errorf("%w: the promotion grant for this repository/SHA/target is held by pending promotion %s", ErrInvalidState, holder)
		}
		return model.PromotionGrant{}, fmt.Errorf("%w: no active promotion grant for this repository/SHA/target", ErrNotFound)
	}
	if err != nil {
		return model.PromotionGrant{}, fmt.Errorf("match promotion grant: %w", err)
	}
	return value, nil
}

// ListPromotionGrants returns grants filtered by repository and optionally SHA
// and active status.
func (s *Store) ListPromotionGrants(ctx context.Context, filter model.PromotionGrantListFilter) ([]model.PromotionGrant, error) {
	now := unixNano(s.now())
	query := `
SELECT id, repo_id, source_sha, target_ref, issuer, issuer_fingerprint, note,
       created_at, expires_at, revoked_by, revoked_at, consumed_by, consumed_at
FROM promotion_grants
WHERE (? = 0 OR repo_id = ?) AND (? = '' OR source_sha = ?)`
	args := []any{filter.RepoID, filter.RepoID, filter.SourceSHA, strings.ToLower(filter.SourceSHA)}
	switch filter.Status {
	case "", "all":
	case "active":
		query += ` AND revoked_at IS NULL AND consumed_at IS NULL AND expires_at > ?`
		args = append(args, now)
	case "revoked":
		query += ` AND revoked_at IS NOT NULL`
	case "consumed":
		query += ` AND revoked_at IS NULL AND consumed_at IS NOT NULL`
	case "expired":
		query += ` AND revoked_at IS NULL AND consumed_at IS NULL AND expires_at <= ?`
		args = append(args, now)
	default:
		return nil, fmt.Errorf("%w: promotion grant status", ErrInvalid)
	}
	if filter.Status == "" && filter.Active != nil {
		if *filter.Active {
			query += ` AND revoked_at IS NULL AND consumed_at IS NULL AND expires_at > ?`
			args = append(args, now)
		} else {
			query += ` AND (revoked_at IS NOT NULL OR consumed_at IS NOT NULL OR expires_at <= ?)`
			args = append(args, now)
		}
	}
	query += ` ORDER BY created_at DESC LIMIT 200`
	rows, err := s.reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list promotion grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var grants []model.PromotionGrant
	for rows.Next() {
		grant, err := scanPromotionGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("scan promotion grant: %w", err)
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list promotion grants: %w", err)
	}
	if grants == nil {
		grants = []model.PromotionGrant{}
	}
	return grants, nil
}

// PromotionGrant reads a single grant by ID.
func (s *Store) PromotionGrant(ctx context.Context, id string) (model.PromotionGrant, error) {
	if strings.TrimSpace(id) == "" {
		return model.PromotionGrant{}, fmt.Errorf("%w: promotion grant ID is required", ErrInvalid)
	}
	value, err := scanPromotionGrant(s.reader().QueryRowContext(ctx, `
SELECT id, repo_id, source_sha, target_ref, issuer, issuer_fingerprint, note,
       created_at, expires_at, revoked_by, revoked_at, consumed_by, consumed_at
FROM promotion_grants WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.PromotionGrant{}, fmt.Errorf("%w: promotion grant", ErrNotFound)
	}
	if err != nil {
		return model.PromotionGrant{}, fmt.Errorf("load promotion grant: %w", err)
	}
	return value, nil
}

func scanPromotionGrant(row rowScanner) (model.PromotionGrant, error) {
	var value model.PromotionGrant
	var created, expires int64
	var revokedAt, consumedAt sql.NullInt64
	if err := scanRowNotFound(row, "PromotionGrant", &value.ID, &value.RepoID, &value.SourceSHA, &value.TargetRef,
		&value.Issuer, &value.IssuerFingerprint, &value.Note,
		&created, &expires, &value.RevokedBy, &revokedAt, &value.ConsumedBy, &consumedAt); err != nil {
		return model.PromotionGrant{}, err
	}
	value.CreatedAt = fromUnixNano(created)
	value.ExpiresAt = fromUnixNano(expires)
	value.RevokedAt = nullableTime(revokedAt)
	value.ConsumedAt = nullableTime(consumedAt)
	return value, nil
}
