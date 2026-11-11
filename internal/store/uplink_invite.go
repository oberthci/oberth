package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/invite"
	"github.com/oberthci/oberth/internal/model"
)

func (s *Store) CreateUplinkInvite(ctx context.Context, actor, digest, identity, address, pin string, scopes []string, ttl time.Duration) (invite.Binding, error) {
	var result invite.Binding
	if actor == "" || invite.ValidateIdentity(identity) != nil || invite.ValidatePin(digest) != nil || invite.ValidatePin(pin) != nil || invite.ValidateURL(address) != nil || ttl <= 0 || ttl > invite.MaximumLifetime {
		return result, ErrInvalid
	}
	scopes, err := s.ResolveUplinkScopes(ctx, scopes)
	if err != nil {
		return result, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return result, err
	}
	now := s.now()
	result = invite.Binding{ID: hex.EncodeToString(id), Identity: identity, ServerURL: address, CAPin: pin, Scope: scopes, ExpiresAt: now.Add(ttl)}
	scopeJSON, err := json.Marshal(scopes)
	if err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO uplink_invites(id,secret_digest,identity,server_url,ca_pin,scope_json,issuer,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, result.ID, digest, identity, address, pin, string(scopeJSON), actor, unixNano(result.ExpiresAt), unixNano(now)); err != nil {
		return result, err
	}
	details, _ := json.Marshal(result)
	if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "uplink.invite.create", ResourceType: "uplink_invite", ResourceID: result.ID, Details: string(details)}, unixNano(now)); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

// InviteEnrollment implements the shared certificate issuance interface inside
// the transaction that consumes the invite and creates its certificate-only uplink.
type InviteEnrollment struct {
	store  *Store
	tx     *sql.Tx
	uplink model.Uplink
}

func (e *InviteEnrollment) Uplink() (model.Uplink, error) { return e.uplink, nil }
func (e *InviteEnrollment) CertificateUplink(_ context.Context, identity, fingerprint string) (model.Uplink, error) {
	if identity != e.uplink.Identity || fingerprint != e.uplink.Fingerprint {
		return model.Uplink{}, ErrNotFound
	}
	return e.uplink, nil
}
func (e *InviteEnrollment) BindClientPin(ctx context.Context, actor string, id int64, credentialID, pin string, expiry time.Time) error {
	if id != e.uplink.ID || credentialID != e.uplink.TokenCredentialID {
		return ErrInvalid
	}
	return e.store.bindClientPin(ctx, e.tx, actor, id, credentialID, pin, expiry)
}

// The first statement takes the write reservation. Failed issuance rolls back
// consumption, registration, pin and audit together; concurrent use has one winner.
func (s *Store) RedeemUplinkInvite(ctx context.Context, digest, address, pin, fingerprint string, issue func(*InviteEnrollment) (string, error)) (invite.Binding, string, error) {
	var binding invite.Binding
	if invite.ValidatePin(digest) != nil || invite.ValidatePin(pin) != nil || invite.ValidateURL(address) != nil || fingerprint == "" || issue == nil {
		return binding, "", ErrInvalid
	}
	// Reject unknown, expired or spent credentials through the reserved read pool.
	// The writer still repeats every predicate atomically for the one winner.
	if err := s.checkUplinkInvite(ctx, digest, address, pin); err != nil {
		return binding, "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return binding, "", err
	}
	defer func() { _ = tx.Rollback() }()
	var scopeJSON, issuer string
	var expiry int64
	now := unixNano(s.now())
	err = tx.QueryRowContext(ctx, `UPDATE uplink_invites SET redeemed_at=? WHERE secret_digest=? AND server_url=? AND ca_pin=? AND redeemed_at IS NULL AND expires_at>? RETURNING id,identity,server_url,ca_pin,scope_json,issuer,expires_at`, now, digest, address, pin, now).Scan(&binding.ID, &binding.Identity, &binding.ServerURL, &binding.CAPin, &scopeJSON, &issuer, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return binding, "", ErrNotFound
	}
	if err != nil {
		return binding, "", err
	}
	if err = json.Unmarshal([]byte(scopeJSON), &binding.Scope); err != nil {
		return binding, "", err
	}
	binding.ExpiresAt = fromUnixNano(expiry)
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM uplinks WHERE identity=? OR fingerprint=?)`, binding.Identity, fingerprint).Scan(&exists); err != nil {
		return binding, "", err
	}
	if exists {
		return binding, "", fmt.Errorf("%w: invite cannot replace an existing uplink", ErrInvalidState)
	}
	u, err := s.registerCertificateUplink(ctx, tx, binding.Identity, model.UplinkSpec{Identity: binding.Identity, Fingerprint: fingerprint, Scope: binding.Scope})
	if err != nil {
		return binding, "", err
	}
	certificate, err := issue(&InviteEnrollment{store: s, tx: tx, uplink: u})
	if err != nil {
		return binding, "", err
	}
	// An issuance callback must actually bind a usable certificate before commit.
	var bound bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM uplink_client_pins WHERE uplink_id=? AND expires_at>?)`, u.ID, now).Scan(&bound); err != nil {
		return binding, "", err
	}
	if !bound || certificate == "" {
		return binding, "", ErrInvalidState
	}
	details, _ := json.Marshal(map[string]any{"identity": binding.Identity, "fingerprint": fingerprint, "scope": binding.Scope, "issuer": issuer, "auth_method": "invite"})
	if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: binding.Identity, Action: "uplink.invite.redeem", ResourceType: "uplink_invite", ResourceID: binding.ID, Details: string(details)}, now); err != nil {
		return binding, "", err
	}
	if err = tx.Commit(); err != nil {
		return binding, "", err
	}
	return binding, certificate, nil
}

func (s *Store) checkUplinkInvite(ctx context.Context, digest, address, pin string) error {
	release, err := acquireAuthSlot(ctx, s.bearerAuthSlots)
	if err != nil {
		return err
	}
	defer release()
	var valid bool
	err = s.authReader().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM uplink_invites WHERE secret_digest=? AND server_url=? AND ca_pin=? AND redeemed_at IS NULL AND expires_at>?)`, digest, address, pin, unixNano(s.now())).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrNotFound
	}
	return nil
}
