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
	"github.com/oberthci/oberth/pkg/periapsis"
)

// DefaultMaxRunSize is the largest admission size a repository without an
// active size grant may run at (#1021). XL reserves the whole host and makes
// every other repository wait for a full drain, so it takes an admin grant.
const DefaultMaxRunSize = periapsis.L

// SizeGrant raises one repository's maximum admission size. It belongs to a
// durable repository ID, never an ambiguous name. Revoking also disables the
// automatic tag boost; granting re-enables it within the maximum size.
type SizeGrant struct {
	RepoID    int64          `json:"repo_id"`
	MaxSize   periapsis.Size `json:"max_size"`
	GrantedBy string         `json:"granted_by"`
	GrantedAt time.Time      `json:"granted_at"`
	RevokedBy string         `json:"revoked_by,omitempty"`
	RevokedAt *time.Time     `json:"revoked_at,omitempty"`
}

// ValidSizeGrantMax reports whether a grant may name this maximum. Only the
// sizes above what every repository may already reach by default, plus L
// itself (an explicit record of the default), are grantable.
func ValidSizeGrantMax(size periapsis.Size) bool {
	return size == periapsis.L || size == periapsis.XL
}

// MaxRunSize is the largest size this repository's runs may be admitted at:
// its active grant's maximum, or DefaultMaxRunSize.
func (s *Store) MaxRunSize(ctx context.Context, repoID int64) (periapsis.Size, error) {
	if repoID <= 0 {
		return "", fmt.Errorf("%w: repository ID is required", ErrInvalid)
	}
	var size string
	err := s.reader().QueryRowContext(ctx, `SELECT max_size FROM size_grants WHERE repo_id=? AND revoked_at IS NULL`, repoID).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultMaxRunSize, nil
	}
	if err != nil {
		return "", err
	}
	return periapsis.Size(size), nil
}

func (s *Store) SizeGrant(ctx context.Context, repoID int64) (SizeGrant, error) {
	var grant SizeGrant
	var size string
	var grantedAt int64
	var revokedAt sql.NullInt64
	err := s.reader().QueryRowContext(ctx, `SELECT repo_id, max_size, granted_by, granted_at, revoked_by, revoked_at FROM size_grants WHERE repo_id=?`, repoID).
		Scan(&grant.RepoID, &size, &grant.GrantedBy, &grantedAt, &grant.RevokedBy, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SizeGrant{}, fmt.Errorf("%w: size grant", ErrNotFound)
	}
	if err != nil {
		return SizeGrant{}, err
	}
	grant.MaxSize = periapsis.Size(size)
	grant.GrantedAt = fromUnixNano(grantedAt)
	if revokedAt.Valid {
		at := fromUnixNano(revokedAt.Int64)
		grant.RevokedAt = &at
	}
	return grant, nil
}

// SetSizeGrant grants (max non-empty) or revokes (max empty) a repository's
// size maximum, with its audit entry in the same transaction. Granting over
// an active grant replaces its maximum; granting the maximum already active,
// or revoking an already revoked grant, is an invalid state. A first revoke
// records an opt-out even when no size grant was ever needed.
func (s *Store) SetSizeGrant(ctx context.Context, repoID int64, actor string, max periapsis.Size) (SizeGrant, error) {
	if repoID <= 0 || strings.TrimSpace(actor) == "" {
		return SizeGrant{}, fmt.Errorf("%w: repository and actor are required", ErrInvalid)
	}
	allow := max != ""
	if allow && !ValidSizeGrantMax(max) {
		return SizeGrant{}, fmt.Errorf("%w: size grant maximum must be %s or %s, got %q", ErrInvalid, periapsis.L, periapsis.XL, max)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SizeGrant{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM repositories WHERE id=?)`, repoID).Scan(&exists); err != nil {
		return SizeGrant{}, err
	}
	if !exists {
		return SizeGrant{}, fmt.Errorf("%w: repository", ErrNotFound)
	}
	var currentSize string
	var revoked sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT max_size, revoked_at FROM size_grants WHERE repo_id=?`, repoID).Scan(&currentSize, &revoked)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SizeGrant{}, err
	}
	active := err == nil && !revoked.Valid
	previous := ""
	if active {
		previous = currentSize
	}
	switch {
	case allow && active && periapsis.Size(currentSize) == max:
		return SizeGrant{}, fmt.Errorf("%w: size grant %s is already active", ErrInvalidState, max)
	case !allow && revoked.Valid:
		return SizeGrant{}, fmt.Errorf("%w: size grant is already revoked", ErrInvalidState)
	case allow:
		_, err = tx.ExecContext(ctx, `INSERT INTO size_grants(repo_id,max_size,granted_by,granted_at) VALUES(?,?,?,?)
ON CONFLICT(repo_id) DO UPDATE SET max_size=excluded.max_size,granted_by=excluded.granted_by,granted_at=excluded.granted_at,revoked_by='',revoked_at=NULL`,
			repoID, string(max), actor, now)
	default:
		_, err = tx.ExecContext(ctx, `INSERT INTO size_grants(repo_id,max_size,granted_by,granted_at,revoked_by,revoked_at) VALUES(?,?,?,?,?,?)
ON CONFLICT(repo_id) DO UPDATE SET revoked_by=excluded.revoked_by,revoked_at=excluded.revoked_at`, repoID, string(DefaultMaxRunSize), actor, now, actor, now)
	}
	if err != nil {
		return SizeGrant{}, err
	}
	details, err := json.Marshal(map[string]any{"repo_id": repoID, "max_size": string(max), "previous_max_size": previous, "automatic_tag_priority_enabled": allow, "previous_automatic_tag_priority_enabled": !revoked.Valid})
	if err != nil {
		return SizeGrant{}, err
	}
	action := "size.grant"
	if !allow {
		action = "size.revoke"
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: action, ResourceType: "repository", ResourceID: fmt.Sprint(repoID), Details: string(details)}, now); err != nil {
		return SizeGrant{}, err
	}
	if err := tx.Commit(); err != nil {
		return SizeGrant{}, err
	}
	return s.SizeGrant(ctx, repoID)
}
