package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// RegisterUpstream atomically creates operator configuration and its audit
// record. Administrative commands must not leave unaudited live mutations.
func (s *Store) RegisterUpstream(ctx context.Context, actor string, spec model.UpstreamSpec) (model.Upstream, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(spec.Name) == "" || strings.TrimSpace(spec.Kind) == "" || strings.TrimSpace(spec.BaseURL) == "" {
		return model.Upstream{}, fmt.Errorf("%w: actor and upstream fields are required", ErrInvalid)
	}
	details, err := json.Marshal(map[string]string{"name": spec.Name, "kind": spec.Kind, "base_url": spec.BaseURL})
	if err != nil {
		return model.Upstream{}, fmt.Errorf("encode upstream audit: %w", err)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Upstream{}, fmt.Errorf("begin upstream registration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
INSERT INTO upstreams(name, kind, base_url, created_at, updated_at) VALUES(?, ?, ?, ?, ?)`,
		spec.Name, spec.Kind, spec.BaseURL, now, now)
	if err != nil {
		return model.Upstream{}, fmt.Errorf("register upstream: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.Upstream{}, fmt.Errorf("read upstream id: %w", err)
	}
	if _, err := appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: "upstream.register", ResourceType: "upstream",
		ResourceID: fmt.Sprint(id), Details: string(details),
	}, now); err != nil {
		return model.Upstream{}, fmt.Errorf("audit upstream registration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Upstream{}, fmt.Errorf("commit upstream registration: %w", err)
	}
	return s.Upstream(ctx, id)
}

// RegisterUplink atomically binds one pending token credential to a public-key
// identity and records who performed the registration. Token activation occurs
// only after this method commits.
func (s *Store) RegisterUplink(ctx context.Context, actor string, spec model.UplinkSpec) (model.Uplink, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(spec.Fingerprint) == "" || strings.TrimSpace(spec.Identity) == "" ||
		strings.TrimSpace(spec.TokenCredentialID) == "" {
		return model.Uplink{}, fmt.Errorf("%w: actor and uplink fields are required", ErrInvalid)
	}
	details, err := json.Marshal(map[string]string{"fingerprint": spec.Fingerprint, "identity": spec.Identity})
	if err != nil {
		return model.Uplink{}, fmt.Errorf("encode uplink audit: %w", err)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Uplink{}, fmt.Errorf("begin uplink registration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx, `
INSERT INTO uplinks(fingerprint, identity, token_credential_id, auth_actor, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(fingerprint) DO UPDATE SET
    identity = excluded.identity,
    token_credential_id = excluded.token_credential_id,
    auth_actor = excluded.auth_actor,
    updated_at = excluded.updated_at
RETURNING id, fingerprint, identity, token_credential_id, auth_actor, created_at, updated_at`,
		spec.Fingerprint, spec.Identity, spec.TokenCredentialID, actor, now, now)
	var value model.Uplink
	var created, updated int64
	if err := row.Scan(&value.ID, &value.Fingerprint, &value.Identity, &value.TokenCredentialID, &value.AuthActor, &created, &updated); err != nil {
		return model.Uplink{}, fmt.Errorf("register uplink: %w", err)
	}
	if _, err := appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: "uplink.register", ResourceType: "uplink",
		ResourceID: fmt.Sprint(value.ID), Details: string(details),
	}, now); err != nil {
		return model.Uplink{}, fmt.Errorf("audit uplink registration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Uplink{}, fmt.Errorf("commit uplink registration: %w", err)
	}
	value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return value, nil
}
