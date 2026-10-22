package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// The audit chain is the durable decision journal; no process-local success
// cache can authorize a rotation after an operator removes its recipe. This
// lookup and the eventual append share the store's immediate write transaction.
func (s *Store) repeatedPinRotationDecision(ctx context.Context, execer auditExecer, spec model.AuditActionSpec) (model.AuditAction, bool, error) {
	if spec.Action != "trusted.pin_rotation.authorized" && spec.Action != "trusted.pin_rotation.refused" {
		return model.AuditAction{}, false, nil
	}
	var details map[string]string
	if err := json.Unmarshal([]byte(spec.Details), &details); err != nil {
		return model.AuditAction{}, false, err
	}
	if details["run_id"] == "" {
		// Preserve append semantics for records from before run-scoped audits.
		return model.AuditAction{}, false, nil
	}
	previous, err := s.scanVerifiedRotationAudit(execer.QueryRowContext(ctx, `
SELECT id, actor, action, resource_type, resource_id, details, previous_sha256, sha256, created_at
FROM audit_actions WHERE resource_type = ? AND resource_id = ?
  AND action IN ('trusted.pin_rotation.authorized', 'trusted.pin_rotation.refused')
  AND json_extract(details, '$.run_id') = ? ORDER BY id DESC LIMIT 1`,
		spec.ResourceType, spec.ResourceID, details["run_id"]))
	if errors.Is(err, sql.ErrNoRows) {
		return model.AuditAction{}, false, nil
	}
	if err != nil {
		return model.AuditAction{}, false, err
	}
	var prior map[string]string
	if err := json.Unmarshal([]byte(previous.Details), &prior); err != nil {
		return model.AuditAction{}, false, err
	}
	if previous.Action != spec.Action || previous.Actor != spec.Actor {
		return model.AuditAction{}, false, nil
	}
	for _, field := range []string{"from_suite_sha256", "from_guest_sha256", "to_suite_sha256", "to_guest_sha256"} {
		if details[field] == "" || details[field] != prior[field] {
			return model.AuditAction{}, false, nil
		}
	}
	// A no-op must still reject a corrupt head, just as appendAuditAction does.
	if _, err := s.scanVerifiedRotationAudit(execer.QueryRowContext(ctx, `
SELECT id, actor, action, resource_type, resource_id, details, previous_sha256, sha256, created_at
FROM audit_actions ORDER BY id DESC LIMIT 1`)); err != nil {
		return model.AuditAction{}, false, err
	}
	return previous, true, nil
}

func (s *Store) scanVerifiedRotationAudit(row *sql.Row) (model.AuditAction, error) {
	var action model.AuditAction
	var createdAt int64
	if err := row.Scan(&action.ID, &action.Actor, &action.Action, &action.ResourceType, &action.ResourceID,
		&action.Details, &action.PreviousSHA256, &action.SHA256, &createdAt); err != nil {
		return action, err
	}
	spec := model.AuditActionSpec{Actor: action.Actor, Action: action.Action, ResourceType: action.ResourceType,
		ResourceID: action.ResourceID, Details: action.Details}
	if len(action.PreviousSHA256) != sha256.Size ||
		!bytes.Equal(action.SHA256, auditActionSHA256(action.ID, action.PreviousSHA256, spec, createdAt, s.protocol.AuditDomain)) {
		return action, fmt.Errorf("pin rotation audit action %d hash is invalid", action.ID)
	}
	action.CreatedAt = fromUnixNano(createdAt)
	return action, nil
}
