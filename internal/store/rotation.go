package store

import (
	"context"
	"database/sql"

	"github.com/oberthci/oberth/internal/stateport"
)

// RotateCredential preserves the selected key's identity, kind, scope and push policy.
func (s *Store) RotateCredential(ctx context.Context, action stateport.Action, input CredentialRotation) error {
	if !validDigest(input.PreviousDigest) || !validDigest(input.Next.Digest) || !input.Next.ExpiresAt.After(s.options.Clock()) {
		return refuse(invalid, nil)
	}
	return s.change(ctx, mutation{action.Actor, "credential.rotate", "key", input.Next.Fingerprint, nil}, func(tx *sql.Tx) error {
		var fingerprint string
		if err := tx.QueryRowContext(ctx, `SELECT c.fingerprint FROM credentials c JOIN keys k ON k.fingerprint=c.fingerprint WHERE c.digest=?
AND c.revoked=0 AND k.revoked=0`, input.PreviousDigest).Scan(&fingerprint); err != nil {
			return refuse(forbidden, err)
		}
		if fingerprint != input.Next.Fingerprint {
			return refuse(forbidden, nil)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO credentials(digest,fingerprint,expires) VALUES(?,?,?)`, input.Next.Digest, fingerprint,
			input.Next.ExpiresAt.UTC().Format(timestampFormat)); err != nil {
			return refuse(conflict, err)
		}
		_, err := tx.ExecContext(ctx, `UPDATE credentials SET revoked=1 WHERE digest=?`, input.PreviousDigest)
		return databaseError(err)
	}, action.Events...)
}

type CredentialRotation struct {
	PreviousDigest string
	Next           stateport.Credential
}
