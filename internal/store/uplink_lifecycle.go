package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// RevokeUplink retains the public registration for lifecycle listings. Rotation
// names an already-issued replacement: no private key or bearer crosses MCP.
func (s *Store) RevokeUplink(ctx context.Context, actor, identity, fingerprint, replacement string) (model.Uplink, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(identity) == "" {
		return model.Uplink{}, fmt.Errorf("%w: actor and identity required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Uplink{}, err
	}
	defer func() { _ = tx.Rollback() }()
	matches, err := matchUplinksByIdentity(ctx, tx, identity, fingerprint)
	if err != nil {
		return model.Uplink{}, err
	}
	if len(matches) == 0 {
		return model.Uplink{}, ErrNotFound
	}
	if len(matches) != 1 {
		return model.Uplink{}, fmt.Errorf("%w: specify fingerprint", ErrAmbiguous)
	}
	value := matches[0]
	value.Scope, err = loadUplinkScopes(ctx, tx, value.ID)
	if err != nil {
		return model.Uplink{}, err
	}
	if replacement != "" {
		replacements, err := matchUplinksByIdentity(ctx, tx, identity, replacement)
		if err != nil {
			return model.Uplink{}, err
		}
		if len(replacements) != 1 || replacements[0].ID == value.ID {
			return model.Uplink{}, fmt.Errorf("%w: a different registered replacement is required", ErrInvalid)
		}
		next := replacements[0]
		scope, err := loadUplinkScopes(ctx, tx, next.ID)
		if err != nil {
			return model.Uplink{}, err
		}
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM token_credentials WHERE id=? AND activated_at IS NOT NULL AND revoked_at IS NULL)`, next.TokenCredentialID).Scan(&active); err != nil {
			return model.Uplink{}, err
		}
		if !active || next.Admin != value.Admin || !slices.Equal(scope, value.Scope) {
			return model.Uplink{}, fmt.Errorf("%w: replacement must be active with identical admin status and scope", ErrInvalid)
		}
	}
	var revoked, activated sql.NullInt64
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT revoked_at, activated_at, EXISTS(SELECT 1 FROM pending_token_credentials p WHERE p.token_credential_id=token_credentials.id) FROM token_credentials WHERE id=?`, value.TokenCredentialID).Scan(&revoked, &activated, &pending); err != nil {
		return model.Uplink{}, err
	}
	if revoked.Valid && !pending {
		return value, nil
	}
	if err := guardLastActiveAdmin(ctx, tx, value.ID); err != nil {
		return model.Uplink{}, err
	}
	now := unixNano(s.now())
	if _, err := revokeTokenCredentialTx(ctx, tx, value.TokenCredentialID, now); err != nil {
		return model.Uplink{}, err
	}
	details, _ := json.Marshal(map[string]string{"identity": identity, "fingerprint": value.Fingerprint, "replacement_fingerprint": replacement})
	action := "uplink.revoke"
	if replacement != "" {
		action = "uplink.rotate"
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: action, ResourceType: "uplink", ResourceID: fmt.Sprint(value.ID), Details: string(details)}, now); err != nil {
		return model.Uplink{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Uplink{}, err
	}
	return value, nil
}

// Both administrative removal and remote revocation must retain a usable
// installation administrator. Run inside the mutation's serialized transaction.
func guardLastActiveAdmin(ctx context.Context, tx *sql.Tx, id int64) error {
	var targetActive, otherActive bool
	const active = `SELECT u.id FROM uplinks u JOIN token_credentials t ON t.id=u.token_credential_id
WHERE u.admin=1 AND t.activated_at IS NOT NULL AND t.revoked_at IS NULL
AND NOT EXISTS(SELECT 1 FROM uplink_scopes sc WHERE sc.uplink_id=u.id)`
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM (`+active+`) WHERE id=?), EXISTS(SELECT 1 FROM (`+active+`) WHERE id<>?)`, id, id).Scan(&targetActive, &otherActive); err != nil {
		return err
	}
	if targetActive && !otherActive {
		return fmt.Errorf("%w: cannot remove or revoke the last active unscoped admin credential", ErrInvalidState)
	}
	return nil
}
