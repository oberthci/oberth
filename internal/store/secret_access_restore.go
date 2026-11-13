package store

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// ReplaceSecretAccess restores an exact grant set in one audited transaction.
// Offline history restore uses this to retain the stopped target's approvals.
func (s *Store) ReplaceSecretAccess(ctx context.Context, actor string, keys []model.SecretAccessKey) error {
	for _, key := range keys {
		if err := validateSecretAccessKey(key, actor); err != nil {
			return err
		}
	}
	if actor == "" {
		return fmt.Errorf("%w: restore actor required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := unixNano(s.now())
	if _, err := tx.ExecContext(ctx, `UPDATE secret_access SET revoked_by=?, revoked_at=? WHERE revoked_at IS NULL`, actor, now); err != nil {
		return err
	}
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO secret_access(repo,workflow,step,secret,approved_by,approved_at) VALUES(?,?,?,?,?,?) ON CONFLICT(repo,workflow,step,secret) WHERE revoked_at IS NULL DO NOTHING`, key.Repo, key.Workflow, key.Step, key.Secret, actor, now); err != nil {
			return err
		}
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "secret_access.restore", ResourceType: "secret_access", ResourceID: "grants", Details: fmt.Sprintf(`{"count":%d}`, len(keys))}, now); err != nil {
		return err
	}
	return tx.Commit()
}
