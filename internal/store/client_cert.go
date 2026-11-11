package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// RegisterCertificateUplink creates a lifecycle record without minting a bearer
// secret. Its public digest is never accepted by bearer authentication.
func (s *Store) RegisterCertificateUplink(ctx context.Context, actor string, spec model.UplinkSpec) (model.Uplink, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(spec.Identity) == "" || strings.TrimSpace(spec.Fingerprint) == "" {
		return model.Uplink{}, ErrInvalid
	}
	scopes, err := normalizeUplinkScopes(spec.Scope)
	if err != nil {
		return model.Uplink{}, err
	}
	if spec.Admin && model.MetricsOnly(scopes) {
		return model.Uplink{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Uplink{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// Never replace a registration or widen its authority during installation.
	matches, err := matchUplinksByIdentity(ctx, tx, spec.Identity, spec.Fingerprint)
	if err != nil {
		return model.Uplink{}, err
	}
	if len(matches) > 0 {
		existing := matches[0]
		existing.Scope, err = loadUplinkScopes(ctx, tx, existing.ID)
		if err != nil {
			return model.Uplink{}, err
		}
		var certificateOnly, active bool
		if err = tx.QueryRowContext(ctx, `SELECT certificate_only,activated_at IS NOT NULL AND revoked_at IS NULL FROM token_credentials WHERE id=?`, existing.TokenCredentialID).Scan(&certificateOnly, &active); err != nil {
			return model.Uplink{}, err
		}
		if !certificateOnly || !active || existing.Admin != spec.Admin || !slices.Equal(existing.Scope, scopes) {
			return model.Uplink{}, fmt.Errorf("%w: existing uplink authority differs", ErrInvalidState)
		}
		return existing, nil
	}
	now := unixNano(s.now())
	digest := sha256.Sum256([]byte(fmt.Sprintf("certificate-only:%s:%s:%d", spec.Identity, spec.Fingerprint, now)))
	id := "cert-" + hex.EncodeToString(digest[:])
	_, err = tx.ExecContext(ctx, `INSERT INTO token_credentials(id,name,digest,created_at,activated_at,certificate_only) VALUES(?,?,?,?,?,1)`, id, spec.Identity, digest[:], now, now)
	if err != nil {
		return model.Uplink{}, err
	}
	var u model.Uplink
	var admin int
	err = tx.QueryRowContext(ctx, `INSERT INTO uplinks(fingerprint,identity,token_credential_id,auth_actor,admin,created_at,updated_at) VALUES(?,?,?,?,?,?,?) RETURNING id,admin`, spec.Fingerprint, spec.Identity, id, actor, spec.Admin, now, now).Scan(&u.ID, &admin)
	if err != nil {
		return u, err
	}
	if err = replaceUplinkScopes(ctx, tx, u.ID, scopes); err != nil {
		return u, err
	}
	details, _ := json.Marshal(map[string]any{"identity": spec.Identity, "fingerprint": spec.Fingerprint, "admin": spec.Admin, "scope": scopes, "auth_method": "cert"})
	if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "uplink.register", ResourceType: "uplink", ResourceID: fmt.Sprint(u.ID), Details: string(details)}, now); err != nil {
		return u, err
	}
	if err = tx.Commit(); err != nil {
		return u, err
	}
	u.Identity = spec.Identity
	u.Fingerprint = spec.Fingerprint
	u.TokenCredentialID = id
	u.Admin = admin != 0
	u.Scope = scopes
	u.AuthActor = actor
	u.CreatedAt = fromUnixNano(now)
	u.UpdatedAt = u.CreatedAt
	return u, nil
}

// BindClientPin is atomic with its issuance receipt, and refuses a removed or
// revoked or replaced registration. A pin cannot migrate to a different uplink.
func (s *Store) BindClientPin(ctx context.Context, actor string, id int64, credentialID, pin string, expiry time.Time) error {
	if actor == "" || credentialID == "" || len(pin) != 64 || !expiry.After(s.now()) {
		return ErrInvalid
	}
	if _, err := hex.DecodeString(pin); err != nil {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var active bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM uplinks u JOIN token_credentials t ON t.id=u.token_credential_id WHERE u.id=? AND t.id=? AND t.activated_at IS NOT NULL AND t.revoked_at IS NULL)`, id, credentialID).Scan(&active); err != nil {
		return err
	}
	if !active {
		return ErrNotFound
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO uplink_client_pins(pin,uplink_id,expires_at,created_at) VALUES(?,?,?,?) ON CONFLICT(pin) DO UPDATE SET expires_at=MAX(expires_at,excluded.expires_at) WHERE uplink_id=excluded.uplink_id`, pin, id, unixNano(expiry), unixNano(s.now()))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: client key belongs to another uplink", ErrInvalid)
	}
	details, _ := json.Marshal(map[string]any{"pin": pin, "expires_at": expiry.UTC()})
	if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "uplink.cert.issue", ResourceType: "uplink", ResourceID: fmt.Sprint(id), Details: string(details)}, unixNano(s.now())); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UplinkByClientPin(ctx context.Context, pin string) (model.Uplink, error) {
	release, err := acquireAuthSlot(ctx, s.bearerAuthSlots)
	if err != nil {
		return model.Uplink{}, err
	}
	defer release()
	tx, err := s.authReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.Uplink{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var u model.Uplink
	var created, updated int64
	err = tx.QueryRowContext(ctx, `SELECT u.id,u.fingerprint,u.identity,u.token_credential_id,u.auth_actor,u.admin,u.created_at,u.updated_at FROM uplink_client_pins p JOIN uplinks u ON u.id=p.uplink_id JOIN token_credentials t ON t.id=u.token_credential_id WHERE p.pin=? AND p.expires_at>? AND t.activated_at IS NOT NULL AND t.revoked_at IS NULL`, pin, unixNano(s.now())).Scan(&u.ID, &u.Fingerprint, &u.Identity, &u.TokenCredentialID, &u.AuthActor, &u.Admin, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	if err != nil {
		return u, err
	}
	if err = validateUplinkBinding(u); err != nil {
		return u, err
	}
	u.Scope, err = loadUplinkScopes(ctx, tx, u.ID)
	if err != nil {
		return u, err
	}
	u.CreatedAt = fromUnixNano(created)
	u.UpdatedAt = fromUnixNano(updated)
	return u, tx.Commit()
}

func (s *Store) CertificateUplink(ctx context.Context, identity, fingerprint string) (model.Uplink, error) {
	tx, err := s.reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
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
	return matches[0], nil
}
