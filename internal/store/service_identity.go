package store

import (
	"context"
	"database/sql"
	"slices"

	"github.com/oberthci/oberth/internal/stateport"
)

// RegisterIdentity binds the key and every offered credential atomically. A
// conflict never returns a credential that was not committed.
func (s *Store) RegisterIdentity(ctx context.Context, a stateport.Action, v stateport.IdentityRegistration) error {
	if err := s.validateIdentity(v); err != nil {
		return err
	}
	return s.change(ctx, mutation{a.Actor, "identity.register", "key", v.Key.Fingerprint, nil}, func(tx *sql.Tx) error {
		k := v.Key
		if _, err := tx.ExecContext(ctx, `INSERT INTO keys(fingerprint,identity,admin,scope,kind,park_on_push,expires_at) VALUES(?,?,?,?,?,?,?)`, k.Fingerprint, k.Identity, k.Admin, encode(k.Scope), k.Kind, k.ParkOnPush, keyExpiry(k.ExpiresAt)); err != nil {
			return refuse(conflict, err)
		}
		if v.SPKI != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO pins VALUES(?,?)`, v.SPKI, k.Fingerprint); err != nil {
				return refuse(conflict, err)
			}
		}
		if v.Credential != nil {
			c := v.Credential
			if _, err := tx.ExecContext(ctx, `INSERT INTO credentials(digest,fingerprint,expires) VALUES(?,?,?)`, c.Digest, c.Fingerprint, c.ExpiresAt.UTC().Format(timestampFormat)); err != nil {
				return refuse(conflict, err)
			}
		}
		return nil
	}, a.Events...)
}
func (s *Store) IdentityKeys(ctx context.Context, identity string) ([]stateport.Key, error) {
	out := []stateport.Key{}
	after := ""
	for {
		rows, err := s.Keys(ctx, KeyFilter{Identity: identity, Page: Page{After: after, Limit: 200}})
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if len(rows) < 200 {
			return out, nil
		}
		after = rows[len(rows)-1].Fingerprint
	}
}
func (s *Store) ReplaceKey(ctx context.Context, a stateport.Action, v stateport.KeyReplacement) error {
	if v.Previous == v.Replacement {
		return refuse(invalid, nil)
	}
	return s.change(ctx, mutation{a.Actor, "key.replace", "key", v.Previous, map[string]string{"replacement": v.Replacement}}, func(tx *sql.Tx) error {
		old, err := readKeyBinding(ctx, tx, v.Previous)
		if err != nil {
			return err
		}
		next, err := readKeyBinding(ctx, tx, v.Replacement)
		if err != nil {
			return err
		}
		if !sameKeyAuthority(old, next) {
			return refuse(forbidden, nil)
		}
		if err := s.protectAdmin(ctx, tx, v.Previous); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE credentials SET revoked=1 WHERE fingerprint=?`, v.Previous); err != nil {
			return databaseError(err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE keys SET revoked=1,revoked_at=coalesce(revoked_at,?) WHERE fingerprint=?`, s.options.Clock().UTC().Format(timestampFormat), v.Previous)
		return databaseError(err)
	}, a.Events...)
}
func readKeyBinding(ctx context.Context, tx *sql.Tx, fingerprint string) (stateport.Key, error) {
	return scanKey(tx.QueryRowContext(ctx, `SELECT fingerprint,identity,admin,scope,kind,park_on_push,revoked,revoked_at,expires_at FROM keys WHERE fingerprint=?`, fingerprint))
}

func (s *Store) validateIdentity(v stateport.IdentityRegistration) error {
	if !validKey(v.Key) || v.Key.Revoked || v.SPKI != "" && !validSPKI(v.SPKI) {
		return refuse(invalid, nil)
	}
	if v.Credential != nil && (!validDigest(v.Credential.Digest) || v.Credential.Fingerprint != v.Key.Fingerprint || !v.Credential.ExpiresAt.After(s.options.Clock())) {
		return refuse(invalid, nil)
	}
	return nil
}

func sameKeyAuthority(old, next stateport.Key) bool {
	return !old.Revoked && !next.Revoked && old.Identity == next.Identity && old.Kind == next.Kind && old.Admin == next.Admin && old.ParkOnPush == next.ParkOnPush && slices.Equal(old.Scope, next.Scope)
}
