package store

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"slices"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// stateport.Invitation contains public enrollment facts and only a secret digest.

func (s *Store) Invite(ctx context.Context, action stateport.Action, input stateport.Invitation) error {
	v := input.Public
	if v.ID == "" || v.Identity == "" || v.ServerURL == "" || v.CAPin == "" || !validDigest(input.Digest) || !v.ExpiresAt.After(s.options.Clock()) {
		return refuse(invalid, nil)
	}
	if !validScope(v.Scope) {
		return refuse(invalid, nil)
	}
	return s.change(ctx, mutation{action.Actor, "invitation.create", "invitation", v.ID, v}, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO invitations(id,digest,value) VALUES(?,?,?)`, v.ID, input.Digest, encode(v))
		if err != nil {
			return refuse(conflict, err)
		}
		return nil
	}, action.Events...)
}

// stateport.Enrollment commits invitation consumption, identity, credential and public pin together.

func (s *Store) Enroll(ctx context.Context, input stateport.Enrollment) error {
	if err := s.validateEnrollment(input); err != nil {
		return err
	}
	return s.change(ctx, mutation{input.Key.Identity, "invitation.consume", "invitation", input.InvitationID, nil}, func(tx *sql.Tx) error {
		var body string
		query := `SELECT value FROM invitations WHERE id=? AND digest=? AND consumed=0
   AND reservation=? AND reserved_until>?`
		if err := tx.QueryRowContext(ctx, query,
			input.InvitationID, input.Digest, input.Reservation,
			s.options.Clock().UTC().Format(timestampFormat),
		).Scan(&body); err != nil {
			return refuse(forbidden, err)
		}
		var invitation model.Invitation
		if err := json.Unmarshal([]byte(body), &invitation); err != nil {
			return refuse(corrupt, err)
		}
		k := input.Key
		if err := s.requireInvitation(invitation, k); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO keys(fingerprint,identity,admin,scope,kind,park_on_push,expires_at) VALUES(?,?,0,?,'agent',1,?)`,
			k.Fingerprint, k.Identity, encode(k.Scope), keyExpiry(k.ExpiresAt))
		if err != nil {
			return refuse(conflict, err)
		}
		if input.Credential.Digest != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO credentials(digest,fingerprint,expires) VALUES(?,?,?)`, input.Credential.Digest,
				k.Fingerprint, input.Credential.ExpiresAt.UTC().Format(timestampFormat)); err != nil {
				return refuse(conflict, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pins VALUES(?,?)`, input.SPKI, k.Fingerprint); err != nil {
			return refuse(conflict, err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE invitations SET consumed=1 WHERE id=?`, input.InvitationID)
		return databaseError(err)
	}, input.Events...)
}

func validSPKI(value string) bool {
	body, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return false
	}
	_, err = x509.ParsePKIXPublicKey(body)
	return err == nil
}

func (s *Store) AddCertificatePin(ctx context.Context, action stateport.Action, v stateport.CertificateBinding) error {
	fingerprint, spki := v.Fingerprint, v.SPKI
	if !validSPKI(spki) || !v.ExpiresAt.After(s.options.Clock()) {
		return refuse(invalid, nil)
	}
	return s.change(ctx, mutation{action.Actor, "certificate.bind", "key", fingerprint, nil}, func(tx *sql.Tx) error {
		var revoked bool
		if err := tx.QueryRowContext(ctx, `SELECT revoked FROM keys WHERE fingerprint=?`, fingerprint).Scan(&revoked); err != nil {
			return databaseError(err)
		}
		if revoked {
			return refuse(forbidden, nil)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO pins VALUES(?,?) ON CONFLICT(spki) DO NOTHING`, spki, fingerprint)
		if err != nil {
			return databaseError(err)
		}
		var bound string
		if err := tx.QueryRowContext(ctx, `SELECT fingerprint FROM pins WHERE spki=?`, spki).Scan(&bound); err != nil {
			return databaseError(err)
		}
		if bound != fingerprint {
			return refuse(conflict, nil)
		}
		_, err = tx.ExecContext(ctx, `UPDATE keys SET expires_at=? WHERE fingerprint=?`, keyExpiry(v.ExpiresAt), fingerprint)
		return databaseError(err)
	}, action.Events...)
}

// AuthenticateCertificate resolves only public pins; certificate verification is caller-owned.
func (s *Store) AuthenticateCertificate(ctx context.Context, protocol int, spki string) (actor.Actor, error) {
	if !validSPKI(spki) || protocol < 0 || protocol >= len(s.protocols) {
		return actor.Actor{}, refuse(invalid, nil)
	}
	select {
	case s.protocols[protocol] <- struct{}{}:
	case <-ctx.Done():
		return actor.Actor{}, ctx.Err()
	}
	defer func() { <-s.protocols[protocol] }()
	var k stateport.Key
	var scope string
	err := s.authReaders.QueryRowContext(ctx, `SELECT k.fingerprint,k.identity,k.admin,k.scope,k.kind,k.park_on_push FROM pins p JOIN keys k ON
k.fingerprint=p.fingerprint WHERE p.spki=? AND k.revoked=0 AND (k.expires_at IS NULL OR k.expires_at>?)`, spki, s.options.Clock().UTC().Format(timestampFormat)).Scan(&k.Fingerprint, &k.Identity, &k.Admin, &scope, &k.Kind, &k.ParkOnPush)
	if err != nil || json.Unmarshal([]byte(scope), &k.Scope) != nil || !validKey(k) {
		return actor.Actor{}, refuse(forbidden, err)
	}
	return actor.Actor{
		AuthMethod:  "cert",
		Identity:    k.Identity,
		Fingerprint: k.Fingerprint,
		Admin:       k.Admin,
		Scope:       k.Scope,
	}, nil
}

func (s *Store) validateEnrollment(input stateport.Enrollment) error {
	if input.Reservation == "" ||
		!validKey(input.Key) ||
		!validDigest(input.Digest) ||
		!validSPKI(input.SPKI) {
		return refuse(invalid, nil)
	}
	if input.Credential.Digest != "" {
		if !validDigest(input.Credential.Digest) || input.Credential.Fingerprint != input.Key.Fingerprint || !input.Credential.ExpiresAt.After(s.options.Clock()) {
			return refuse(invalid, nil)
		}
	} else if input.Credential.Fingerprint != "" || !input.Credential.ExpiresAt.IsZero() {
		return refuse(invalid, nil)
	}
	return nil
}

func (s *Store) requireInvitation(invitation model.Invitation, k stateport.Key) error {
	if !invitation.ExpiresAt.After(s.options.Clock()) || invitation.Identity != k.Identity || !slices.Equal(invitation.Scope, k.Scope) ||
		k.Admin || k.Kind != "agent" || k.Revoked {
		return refuse(invitationExpired, nil)
	}
	return nil
}
