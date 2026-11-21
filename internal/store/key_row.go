package store

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
)

func scanKey(row interface{ Scan(...any) error }) (stateport.Key, error) {
	var k stateport.Key
	var scope string
	var revokedAt, expiresAt sql.NullString
	if err := row.Scan(&k.Fingerprint, &k.Identity, &k.Admin, &scope, &k.Kind, &k.ParkOnPush, &k.Revoked, &revokedAt, &expiresAt); err != nil {
		return k, databaseError(err)
	}
	if json.Unmarshal([]byte(scope), &k.Scope) != nil || !validKey(k) {
		return k, refuse(corrupt, nil)
	}
	if revokedAt.Valid {
		at, err := time.Parse(time.RFC3339Nano, revokedAt.String)
		if err != nil || !k.Revoked {
			return k, refuse(corrupt, err)
		}
		k.RevokedAt = &at
	}
	if expiresAt.Valid {
		var err error
		k.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt.String)
		if err != nil {
			return k, refuse(corrupt, err)
		}
	}
	return k, nil
}

func keyExpiry(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return at.UTC().Format(timestampFormat)
}
