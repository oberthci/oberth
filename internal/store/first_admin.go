package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// FirstAdminEnrollment keeps token activation, certificate pins and the exec
// recovery receipt in one transaction. It is used only by create-first-admin.
type FirstAdminEnrollment struct {
	*Store
	tx     *sql.Tx
	actor  string
	spec   model.UplinkSpec
	policy model.UplinkTokenPolicy
	uplink model.Uplink
}

func (e *FirstAdminEnrollment) CreateTokenCredential(ctx context.Context, spec model.TokenCredentialSpec) (model.TokenCredential, error) {
	return e.createTokenCredential(ctx, e.tx, spec, e.policy)
}
func (e *FirstAdminEnrollment) ActivateTokenCredential(ctx context.Context, id string) (model.TokenCredential, error) {
	return e.activateTokenCredential(ctx, e.tx, id)
}
func (e *FirstAdminEnrollment) Register(ctx context.Context, credential model.TokenCredential) error {
	e.spec.TokenCredentialID = credential.ID
	u, err := e.registerUplink(ctx, e.tx, e.actor, e.spec, true)
	if err != nil {
		return err
	}
	if err := replaceUplinkScopes(ctx, e.tx, u.ID, nil); err != nil {
		return err
	}
	u.Scope = nil
	e.uplink = u
	return nil
}
func (e *FirstAdminEnrollment) CertificateUplink(_ context.Context, identity, fingerprint string) (model.Uplink, error) {
	if identity != e.uplink.Identity || fingerprint != e.uplink.Fingerprint {
		return model.Uplink{}, ErrNotFound
	}
	return e.uplink, nil
}
func (e *FirstAdminEnrollment) BindClientPin(ctx context.Context, actor string, id int64, credential, pin string, expiry time.Time) error {
	if id != e.uplink.ID || credential != e.uplink.TokenCredentialID {
		return ErrInvalid
	}
	return e.bindClientPin(ctx, e.tx, actor, id, credential, pin, expiry)
}

// CreateFirstAdmin permits recovery when no active unscoped admin exists, or
// redelivery to the very same identity, SSH key and installer public key.
func (s *Store) CreateFirstAdmin(ctx context.Context, actor, identity, fingerprint, installerPin string, bearer bool, policy model.UplinkTokenPolicy, issue func(*FirstAdminEnrollment) error) error {
	if actor == "" || identity == "" || fingerprint == "" || len(installerPin) != 64 || issue == nil {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Acquire the write reservation before checking eligibility, including an empty DB.
	if _, err := tx.ExecContext(ctx, `UPDATE uplinks SET updated_at=updated_at WHERE id=-1`); err != nil {
		return err
	}
	if err := firstAdminEligible(ctx, tx, identity, fingerprint, installerPin, unixNano(s.now())); err != nil {
		return err
	}
	e := &FirstAdminEnrollment{Store: s, tx: tx, actor: actor, policy: policy, spec: model.UplinkSpec{Identity: identity, Fingerprint: fingerprint, Admin: true}}
	if err := issue(e); err != nil {
		return err
	}
	var bound bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM uplink_client_pins p JOIN token_credentials t ON t.id=? WHERE p.uplink_id=? AND p.pin=? AND p.expires_at>? AND t.activated_at IS NOT NULL AND t.revoked_at IS NULL)`, e.uplink.TokenCredentialID, e.uplink.ID, installerPin, unixNano(s.now())).Scan(&bound); err != nil {
		return err
	}
	if !bound {
		return ErrInvalidState
	}
	if !bearer {
		if _, err := tx.ExecContext(ctx, `UPDATE token_credentials SET certificate_only=1 WHERE id=?`, e.uplink.TokenCredentialID); err != nil {
			return err
		}
	}
	details, _ := json.Marshal(map[string]string{"identity": identity, "fingerprint": fingerprint, "installer_pin": installerPin})
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "uplink.first-admin", ResourceType: "uplink", ResourceID: fmt.Sprint(e.uplink.ID), Details: string(details)}, unixNano(s.now())); err != nil {
		return err
	}
	return tx.Commit()
}

func firstAdminEligible(ctx context.Context, tx *sql.Tx, identity, fingerprint, pin string, now int64) error {
	var active, same bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(`+activeUnscopedAdmins+`), EXISTS(SELECT 1 FROM (`+activeUnscopedAdmins+`) a JOIN uplinks u ON u.id=a.id JOIN uplink_client_pins p ON p.uplink_id=u.id WHERE u.identity=? AND u.fingerprint=? AND p.pin=?)`, now, now, now, now, identity, fingerprint, pin).Scan(&active, &same); err != nil {
		return err
	}
	if active && !same {
		return fmt.Errorf("%w: an active unscoped admin exists; retry with the original installer key or use authenticated administrative APIs", ErrInvalidState)
	}
	return nil
}
