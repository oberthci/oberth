package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oberthci/oberth/internal/stateport"
)

// InstallAdmin atomically replaces the marked install identity and its public
// pin. Authentication continues to use the ordinary keys, credentials and pins.
func (s *Store) InstallAdmin(ctx context.Context, a stateport.Action, v stateport.IdentityRegistration) error {
	if err := s.validateIdentity(v); err != nil {
		return err
	}
	if !installAuthority(v.Key) || v.Credential != nil || v.SPKI == "" {
		return refuse(invalid, nil)
	}
	details := map[string]any{"key": v.Key, "spki": v.SPKI}
	return s.change(ctx, mutation{a.Actor, "identity.install", "installation", "admin", details}, func(tx *sql.Tx) error {
		previous, err := readInstallAdmin(ctx, tx)
		if err != nil {
			return err
		}
		details["previous_fingerprint"] = previous.Key.Fingerprint
		if sameInstallAdmin(previous, v) {
			return unchanged{}
		}
		if err := checkInstallKey(ctx, tx, v.Key); err != nil {
			return err
		}
		if err := s.retireInstallAdmin(ctx, tx, previous.Key.Fingerprint); err != nil {
			return err
		}
		if err := writeInstallAdmin(ctx, tx, v); err != nil {
			return err
		}
		return nil
	}, a.Events...)
}
func installAuthority(k stateport.Key) bool {
	return k.Admin && k.Kind == "installer" && len(k.Scope) == 0 && !k.ParkOnPush && !k.Revoked
}
func readInstallAdmin(ctx context.Context, tx *sql.Tx) (stateport.IdentityRegistration, error) {
	var v stateport.IdentityRegistration
	err := tx.QueryRowContext(ctx, `SELECT fingerprint,spki FROM install_admin WHERE singleton=1`).Scan(&v.Key.Fingerprint, &v.SPKI)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return v, databaseError(err)
	}
	v.Key, err = readKeyBinding(ctx, tx, v.Key.Fingerprint)
	return v, err
}
func checkInstallKey(ctx context.Context, tx *sql.Tx, k stateport.Key) error {
	old, err := readKeyBinding(ctx, tx, k.Fingerprint)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if old.Kind != "installer" || old.Identity != k.Identity {
		return refuse(conflict, nil)
	}
	return nil
}
func (s *Store) retireInstallAdmin(ctx context.Context, tx *sql.Tx, fingerprint string) error {
	if fingerprint == "" {
		return nil
	}
	for _, query := range []string{`UPDATE credentials SET revoked=1 WHERE fingerprint=?`, `DELETE FROM pins WHERE fingerprint=?`} {
		if _, err := tx.ExecContext(ctx, query, fingerprint); err != nil {
			return databaseError(err)
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE keys SET revoked=1,revoked_at=coalesce(revoked_at,?) WHERE fingerprint=?`, s.options.Clock().UTC().Format(timestampFormat), fingerprint)
	return databaseError(err)
}
func writeInstallAdmin(ctx context.Context, tx *sql.Tx, v stateport.IdentityRegistration) error {
	k := v.Key
	_, err := tx.ExecContext(ctx, `INSERT INTO keys(fingerprint,identity,admin,scope,kind,park_on_push) VALUES(?,?,1,'[]','installer',0)
ON CONFLICT(fingerprint) DO UPDATE SET admin=1,scope='[]',revoked=0,revoked_at=NULL,park_on_push=0`, k.Fingerprint, k.Identity)
	if err != nil {
		return databaseError(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO pins VALUES(?,?)`, v.SPKI, k.Fingerprint); err != nil {
		return refuse(conflict, err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO install_admin VALUES(1,?,?) ON CONFLICT(singleton) DO UPDATE SET fingerprint=excluded.fingerprint,spki=excluded.spki`, k.Fingerprint, v.SPKI)
	return databaseError(err)
}

func sameInstallAdmin(old, next stateport.IdentityRegistration) bool {
	return old.SPKI == next.SPKI && old.Key.Fingerprint == next.Key.Fingerprint && old.Key.Identity == next.Key.Identity && installAuthority(old.Key)
}
