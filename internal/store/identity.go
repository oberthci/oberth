package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/stateport"
)

// stateport.Key binds one public fingerprint to immutable identity and push policy.

func validKey(k stateport.Key) bool {
	if strings.TrimSpace(k.Identity) == "" || strings.TrimSpace(k.Fingerprint) == "" {
		return false
	}
	if k.Kind != "agent" && k.Kind != "operator" && k.Kind != "installer" {
		return false
	}
	return validScope(k.Scope)
}
func validScope(upstreams []string) bool {
	if len(upstreams) > 64 {
		return false
	}
	for _, name := range upstreams {
		if reponame.ValidateUpstreamName(name) != nil {
			return false
		}
	}
	return true
}

// RegisterKey preserves every existing authority binding. Only SetKeyScope widens scope.
func (s *Store) RegisterKey(ctx context.Context, action stateport.Action, key stateport.Key) error {
	who := action.Actor

	if !validKey(key) || key.Revoked {
		return refuse(invalid, nil)
	}
	return s.change(ctx, mutation{who, "key.register", "key", key.Fingerprint, nil}, func(tx *sql.Tx) error {
		var identity, kind string
		err := tx.QueryRowContext(ctx, `SELECT identity,kind FROM keys WHERE fingerprint=?`, key.Fingerprint).Scan(&identity, &kind)
		if err == nil {
			if identity != key.Identity || kind != key.Kind {
				return refuse(conflict, nil)
			}
			return unchanged{}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return databaseError(err)
		}
		scope, _ := json.Marshal(key.Scope)
		_, err = tx.ExecContext(ctx, `INSERT INTO keys(fingerprint,identity,admin,scope,kind,park_on_push,expires_at) VALUES(?,?,?,?,?,?,?)`,
			key.Fingerprint, key.Identity, key.Admin, string(scope), key.Kind, key.ParkOnPush, keyExpiry(key.ExpiresAt))
		return databaseError(err)
	}, action.Events...)
}

func (s *Store) SetKeyScope(ctx context.Context, action stateport.Action, fingerprint string, scope []string) error {
	who := action.Actor

	if fingerprint == "" || !validScope(scope) {
		return refuse(invalid, nil)
	}
	details := map[string]any{"scope": scope}
	return s.change(ctx, mutation{who, "key.scope", "key", fingerprint, details}, func(tx *sql.Tx) error {
		var previous string
		if err := tx.QueryRowContext(ctx, `SELECT scope FROM keys WHERE fingerprint=?`, fingerprint).Scan(&previous); err != nil {
			return databaseError(err)
		}
		details["previous"] = json.RawMessage(previous)
		if len(scope) > 0 {
			if err := s.protectAdmin(ctx, tx, fingerprint); err != nil {
				return err
			}
		}
		body, _ := json.Marshal(scope)
		_, err := tx.ExecContext(ctx, `UPDATE keys SET scope=? WHERE fingerprint=?`, string(body), fingerprint)
		return databaseError(err)
	}, action.Events...)
}

func (s *Store) protectAdmin(ctx context.Context, tx *sql.Tx, fingerprint string) error {
	var active bool
	err := tx.QueryRowContext(ctx, `SELECT admin=1 AND revoked=0 AND scope IN ('null','[]') FROM keys WHERE fingerprint=?`, fingerprint).Scan(&active)
	if err != nil {
		return databaseError(err)
	}
	if !active {
		return nil
	}
	available, err := s.hasOtherAdmin(ctx, tx, fingerprint)
	if err != nil {
		return err
	}
	if !available {
		return refuse(lastAdmin, nil)
	}
	return nil
}

func (s *Store) hasOtherAdmin(ctx context.Context, tx *sql.Tx, fingerprint string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.expires,EXISTS(SELECT 1 FROM pins p WHERE p.fingerprint=k.fingerprint) FROM keys k LEFT
JOIN credentials c ON c.fingerprint=k.fingerprint AND c.revoked=0 WHERE k.fingerprint<>? AND k.admin=1
AND k.revoked=0 AND k.scope IN ('null','[]') AND (k.expires_at IS NULL OR k.expires_at>?)`, fingerprint, s.options.Clock().UTC().Format(timestampFormat))
	if err != nil {
		return false, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var expiry sql.NullString
		var pinned bool
		if err := rows.Scan(&expiry, &pinned); err != nil {
			return false, databaseError(err)
		}
		if pinned {
			return true, nil
		}
		expires, err := time.Parse(time.RFC3339Nano, expiry.String)
		if err == nil && s.options.Clock().Before(expires) {
			return true, nil
		}
	}
	return false, databaseError(rows.Err())
}

func (s *Store) RevokeKey(ctx context.Context, action stateport.Action, fingerprint string) error {
	who := action.Actor

	return s.change(ctx, mutation{who, "key.revoke", "key", fingerprint, nil}, func(tx *sql.Tx) error {
		if err := s.protectAdmin(ctx, tx, fingerprint); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE credentials SET revoked=1 WHERE fingerprint=?`, fingerprint); err != nil {
			return databaseError(err)
		}
		_, err := tx.ExecContext(ctx, `UPDATE keys SET revoked=1,revoked_at=coalesce(revoked_at,?) WHERE fingerprint=?`, s.options.Clock().UTC().Format(timestampFormat), fingerprint)
		return databaseError(err)
	}, action.Events...)
}

// stateport.Credential stores only a digest, fixed expiry and its public-key binding.

func (s *Store) AddCredential(ctx context.Context, action stateport.Action, c stateport.Credential) error {
	who := action.Actor

	if !validDigest(c.Digest) || !c.ExpiresAt.After(s.options.Clock()) {
		return refuse(invalid, nil)
	}
	return s.change(ctx, mutation{who, "credential.add", "key", c.Fingerprint, nil}, func(tx *sql.Tx) error {
		var revoked bool
		if err := tx.QueryRowContext(ctx, `SELECT revoked FROM keys WHERE fingerprint=?`, c.Fingerprint).Scan(&revoked); err != nil {
			return databaseError(err)
		}
		if revoked {
			return refuse(forbidden, nil)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO credentials(digest,fingerprint,expires) VALUES(?,?,?)`, c.Digest, c.Fingerprint,
			c.ExpiresAt.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return refuse(conflict, err)
		}
		return nil
	}, action.Events...)
}

// Authenticate reads identity and scope together using the independent reserve.
// Protocol 0 and 1 each use at most three of the four reserved connections.
func (s *Store) Authenticate(ctx context.Context, protocol int, digest string) (actor.Actor, error) {
	if protocol < 0 || protocol >= len(s.protocols) || !validDigest(digest) {
		return actor.Actor{}, refuse(invalid, nil)
	}
	select {
	case s.protocols[protocol] <- struct{}{}:
	case <-ctx.Done():
		return actor.Actor{}, ctx.Err()
	}
	defer func() { <-s.protocols[protocol] }()
	tx, err := s.authReaders.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return actor.Actor{}, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	var k stateport.Key
	var scope, expiry string
	err = tx.QueryRowContext(ctx, `SELECT k.fingerprint,k.identity,k.admin,k.scope,k.kind,k.park_on_push,c.expires FROM credentials
c JOIN keys k ON k.fingerprint=c.fingerprint WHERE c.digest=? AND c.revoked=0 AND k.revoked=0 AND (k.expires_at IS NULL OR k.expires_at>?)`, digest, s.options.Clock().UTC().Format(timestampFormat)).Scan(&k.Fingerprint, &k.Identity, &k.Admin,
		&scope, &k.Kind, &k.ParkOnPush, &expiry)
	if err != nil {
		return actor.Actor{}, refuse(forbidden, err)
	}
	if err := s.validAuthentication(&k, scope, expiry); err != nil {
		return actor.Actor{}, err
	}
	select {
	case s.uses <- digest:
	default:
	}
	return actor.Actor{
		AuthMethod:  "bearer",
		Identity:    k.Identity,
		Fingerprint: k.Fingerprint,
		Admin:       k.Admin,
		Scope:       k.Scope,
	}, nil
}

func (s *Store) validAuthentication(k *stateport.Key, scope, expiry string) error {
	expires, err := time.Parse(time.RFC3339Nano, expiry)
	if err != nil || !s.options.Clock().Before(expires) || json.Unmarshal([]byte(scope), &k.Scope) != nil || !validKey(*k) {
		return refuse(forbidden, err)
	}
	return nil
}

// stateport.Key reads the selected key's explicit kind and push policy without caching authority.
func (s *Store) Key(ctx context.Context, fingerprint string) (stateport.Key, error) {
	return scanKey(s.readers.QueryRowContext(ctx, `SELECT fingerprint,identity,admin,scope,kind,park_on_push,revoked,revoked_at,expires_at FROM keys WHERE fingerprint=?`, fingerprint))
}
