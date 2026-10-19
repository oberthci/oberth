package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// KubeVirtGrant belongs to a durable repository ID, never an ambiguous name.
type KubeVirtGrant struct {
	RepoID    int64      `json:"repo_id"`
	GrantedBy string     `json:"granted_by"`
	GrantedAt time.Time  `json:"granted_at"`
	RevokedBy string     `json:"revoked_by,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

func (s *Store) ActiveKubeVirtGrant(ctx context.Context, repoID int64) (bool, error) {
	if repoID <= 0 {
		return false, fmt.Errorf("%w: repository ID is required", ErrInvalid)
	}
	var active bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM kubevirt_grants WHERE repo_id=? AND revoked_at IS NULL)`, repoID).Scan(&active)
	return active, err
}

func (s *Store) KubeVirtGrant(ctx context.Context, repoID int64) (KubeVirtGrant, error) {
	grant, err := scanKubeVirtGrant(s.db.QueryRowContext(ctx, `SELECT repo_id, granted_by, granted_at, revoked_by, revoked_at FROM kubevirt_grants WHERE repo_id=?`, repoID))
	if errors.Is(err, sql.ErrNoRows) {
		return KubeVirtGrant{}, fmt.Errorf("%w: KubeVirt grant", ErrNotFound)
	}
	return grant, err
}

func scanKubeVirtGrant(row *sql.Row) (KubeVirtGrant, error) {
	var grant KubeVirtGrant
	var grantedAt int64
	var revokedAt sql.NullInt64
	if err := row.Scan(&grant.RepoID, &grant.GrantedBy, &grantedAt, &grant.RevokedBy, &revokedAt); err != nil {
		return KubeVirtGrant{}, err
	}
	grant.GrantedAt = fromUnixNano(grantedAt)
	if revokedAt.Valid {
		at := fromUnixNano(revokedAt.Int64)
		grant.RevokedAt = &at
	}
	return grant, nil
}

// SetKubeVirtGrant changes the capability and its audit entry in one transaction.
func (s *Store) SetKubeVirtGrant(ctx context.Context, repoID int64, actor string, allow bool) (KubeVirtGrant, error) {
	if repoID <= 0 || strings.TrimSpace(actor) == "" {
		return KubeVirtGrant{}, fmt.Errorf("%w: repository and actor are required", ErrInvalid)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return KubeVirtGrant{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM repositories WHERE id=?)`, repoID).Scan(&exists); err != nil {
		return KubeVirtGrant{}, err
	}
	if !exists {
		return KubeVirtGrant{}, fmt.Errorf("%w: repository", ErrNotFound)
	}
	var current sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT revoked_at FROM kubevirt_grants WHERE repo_id=?`, repoID).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return KubeVirtGrant{}, err
	}
	if allow && err == nil && !current.Valid {
		return KubeVirtGrant{}, fmt.Errorf("%w: KubeVirt grant is already active", ErrInvalidState)
	}
	if !allow && (errors.Is(err, sql.ErrNoRows) || current.Valid) {
		return KubeVirtGrant{}, fmt.Errorf("%w: KubeVirt grant is not active", ErrInvalidState)
	}
	if allow {
		_, err = tx.ExecContext(ctx, `INSERT INTO kubevirt_grants(repo_id,granted_by,granted_at) VALUES(?,?,?) ON CONFLICT(repo_id) DO UPDATE SET granted_by=excluded.granted_by,granted_at=excluded.granted_at,revoked_by='',revoked_at=NULL`, repoID, actor, now)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE kubevirt_grants SET revoked_by=?,revoked_at=? WHERE repo_id=? AND revoked_at IS NULL`, actor, now, repoID)
	}
	if err != nil {
		return KubeVirtGrant{}, err
	}
	details, err := json.Marshal(map[string]any{"repo_id": repoID, "allow": allow})
	if err != nil {
		return KubeVirtGrant{}, err
	}
	action := "kubevirt.grant"
	if !allow {
		action = "kubevirt.revoke"
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: action, ResourceType: "repository", ResourceID: fmt.Sprint(repoID), Details: string(details)}, now); err != nil {
		return KubeVirtGrant{}, err
	}
	if err := tx.Commit(); err != nil {
		return KubeVirtGrant{}, err
	}
	return s.KubeVirtGrant(ctx, repoID)
}
