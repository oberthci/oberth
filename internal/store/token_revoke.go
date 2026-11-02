package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Revocation removes activation capability in the same transaction, including
// credentials whose pending representation already carries a revoked time.
func revokeTokenCredentialTx(ctx context.Context, tx *sql.Tx, id string, now int64) (bool, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM pending_token_credentials WHERE token_credential_id=?`, id)
	if err != nil {
		return false, fmt.Errorf("clear pending credential: %w", err)
	}
	pending, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	result, err = tx.ExecContext(ctx, `UPDATE token_credentials SET revoked_at=? WHERE id=? AND (revoked_at IS NULL OR ? > 0)`, now, id, pending)
	if err != nil {
		return false, fmt.Errorf("revoke credential: %w", err)
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}
