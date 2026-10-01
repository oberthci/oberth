package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// grantRepoPattern matches valid characters for the repo field of a grant.
// Grant repos are qualified names (upstream/org/repo) that flow into per-repo
// Vault policy HCL construction; quotes, backslashes, newlines, and control
// characters would escape the HCL string boundary and inject arbitrary policy
// rules (issue #416, same class as #411 and #414).
var grantRepoPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// grantSecretPattern matches valid characters for the secret field of a grant.
// Grant secrets are eventually interpolated into Vault policy HCL path strings
// by the installer; quotes, backslashes, newlines, and control characters
// would escape the HCL string boundary and inject arbitrary policy rules
// (issue #414).
var grantSecretPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// SecretAccessGrant records an approval for a (repo, step, secret) triple. If
// a revoked grant already exists for the same triple, a new active row is
// inserted. If an active grant already exists, it is returned unchanged.
type SecretAccessGrant struct {
	ID         int64
	Repo       string
	Step       string
	Secret     string
	ApprovedBy string
	ApprovedAt time.Time
	RevokedBy  string
	RevokedAt  *time.Time
}

// SecretAccessList returns all active (and optionally revoked) grants for a
// repository, ordered by step then secret. When repo is empty, all repos are
// returned.
func (s *Store) SecretAccessList(ctx context.Context, repo string, includeRevoked bool) ([]SecretAccessGrant, error) {
	var query string
	var args []any
	if includeRevoked {
		if strings.TrimSpace(repo) == "" {
			query = `SELECT id, repo, step, secret, approved_by, approved_at, revoked_by, revoked_at FROM secret_access ORDER BY repo, step, secret, id`
		} else {
			query = `SELECT id, repo, step, secret, approved_by, approved_at, revoked_by, revoked_at FROM secret_access WHERE repo = ? ORDER BY step, secret, id`
			args = append(args, repo)
		}
	} else {
		if strings.TrimSpace(repo) == "" {
			query = `SELECT id, repo, step, secret, approved_by, approved_at, revoked_by, revoked_at FROM secret_access WHERE revoked_at IS NULL ORDER BY repo, step, secret, id`
		} else {
			query = `SELECT id, repo, step, secret, approved_by, approved_at, revoked_by, revoked_at FROM secret_access WHERE repo = ? AND revoked_at IS NULL ORDER BY step, secret, id`
			args = append(args, repo)
		}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list secret access: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var grants []SecretAccessGrant
	for rows.Next() {
		grant, scanErr := scanSecretAccess(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan secret access: %w", scanErr)
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list secret access: %w", err)
	}
	return grants, nil
}

// Grant records an approval for a (repo, step, secret) triple. If an active
// grant already exists it is returned without modification. The actor is the
// identity of the administrator granting access. The mutation and its audit
// action are committed in a single transaction; a failed audit append rolls
// back the grant. Duplicate-tolerant: INSERT ... ON CONFLICT ... WHERE
// revoked_at IS NULL DO NOTHING targets the partial unique index so a
// concurrent CLI+reconciler race cannot surface a spurious constraint error
// for a grant that is already durably in effect.
func (s *Store) Grant(ctx context.Context, repo, step, secret, actor string) (SecretAccessGrant, error) {
	if strings.TrimSpace(repo) == "" || strings.TrimSpace(step) == "" || strings.TrimSpace(secret) == "" || strings.TrimSpace(actor) == "" {
		return SecretAccessGrant{}, fmt.Errorf("%w: repo, step, secret, and actor are required", ErrInvalid)
	}
	if !grantRepoPattern.MatchString(repo) {
		return SecretAccessGrant{}, fmt.Errorf("%w: repo %q contains characters outside [A-Za-z0-9._/-]; refusing to persist (HCL injection risk)", ErrInvalid, repo)
	}
	if !grantSecretPattern.MatchString(secret) {
		return SecretAccessGrant{}, fmt.Errorf("%w: secret %q contains characters outside [A-Za-z0-9._/-]; refusing to persist (HCL injection risk)", ErrInvalid, secret)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SecretAccessGrant{}, fmt.Errorf("begin grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
INSERT INTO secret_access(repo, step, secret, approved_by, approved_at)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(repo, step, secret) WHERE revoked_at IS NULL DO NOTHING`,
		repo, step, secret, actor, now)
	if err != nil {
		return SecretAccessGrant{}, fmt.Errorf("grant secret access: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return SecretAccessGrant{}, fmt.Errorf("check grant insertion: %w", err)
	}
	if affected == 0 {
		// Active grant already exists — return it without a new audit action.
		existing, scanErr := scanSecretAccess(tx.QueryRowContext(ctx, `
SELECT id, repo, step, secret, approved_by, approved_at, revoked_by, revoked_at
FROM secret_access WHERE repo = ? AND step = ? AND secret = ? AND revoked_at IS NULL`,
			repo, step, secret))
		if scanErr != nil {
			return SecretAccessGrant{}, fmt.Errorf("read existing grant: %w", scanErr)
		}
		return existing, nil
	}
	id, err := result.LastInsertId()
	if err != nil {
		return SecretAccessGrant{}, fmt.Errorf("read grant id: %w", err)
	}
	details, err := json.Marshal(map[string]string{"repo": repo, "step": step, "secret": secret})
	if err != nil {
		return SecretAccessGrant{}, fmt.Errorf("encode grant audit: %w", err)
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: "secret_access.grant", ResourceType: "secret_access",
		ResourceID: fmt.Sprint(id), Details: string(details),
	}, now); err != nil {
		return SecretAccessGrant{}, fmt.Errorf("audit grant: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SecretAccessGrant{}, fmt.Errorf("commit grant: %w", err)
	}
	return SecretAccessGrant{
		ID: id, Repo: repo, Step: step, Secret: secret,
		ApprovedBy: actor, ApprovedAt: fromUnixNano(now),
	}, nil
}

// Revoke marks the active grant for (repo, step, secret) as revoked. Returns
// ErrNotFound if no active grant exists. The mutation and its audit action are
// committed in a single transaction; a failed audit append rolls back the
// revocation.
func (s *Store) Revoke(ctx context.Context, repo, step, secret, actor string) (SecretAccessGrant, error) {
	if strings.TrimSpace(repo) == "" || strings.TrimSpace(step) == "" || strings.TrimSpace(secret) == "" || strings.TrimSpace(actor) == "" {
		return SecretAccessGrant{}, fmt.Errorf("%w: repo, step, secret, and actor are required", ErrInvalid)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SecretAccessGrant{}, fmt.Errorf("begin revocation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	grant, err := scanSecretAccess(tx.QueryRowContext(ctx, `
UPDATE secret_access SET revoked_by = ?, revoked_at = ?
WHERE repo = ? AND step = ? AND secret = ? AND revoked_at IS NULL
RETURNING id, repo, step, secret, approved_by, approved_at, revoked_by, revoked_at`,
		actor, now, repo, step, secret))
	if errors.Is(err, sql.ErrNoRows) {
		return SecretAccessGrant{}, fmt.Errorf("%w: no active grant for %s/%s/%s", ErrNotFound, repo, step, secret)
	}
	if err != nil {
		return SecretAccessGrant{}, fmt.Errorf("revoke secret access: %w", err)
	}
	details, err := json.Marshal(map[string]string{"repo": repo, "step": step, "secret": secret})
	if err != nil {
		return SecretAccessGrant{}, fmt.Errorf("encode revocation audit: %w", err)
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: "secret_access.revoke", ResourceType: "secret_access",
		ResourceID: fmt.Sprint(grant.ID), Details: string(details),
	}, now); err != nil {
		return SecretAccessGrant{}, fmt.Errorf("audit revocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SecretAccessGrant{}, fmt.Errorf("commit revocation: %w", err)
	}
	return grant, nil
}

// SecretAccessCheck reports whether an active (non-revoked) grant exists for
// the given (repo, step, secret) triple.
func (s *Store) SecretAccessCheck(ctx context.Context, repo, step, secret string) (bool, error) {
	if strings.TrimSpace(repo) == "" || strings.TrimSpace(step) == "" || strings.TrimSpace(secret) == "" {
		return false, fmt.Errorf("%w: repo, step, and secret are required", ErrInvalid)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `
SELECT count(*) FROM secret_access
WHERE repo = ? AND step = ? AND secret = ? AND revoked_at IS NULL`,
		repo, step, secret).Scan(&count); err != nil {
		return false, fmt.Errorf("check secret access: %w", err)
	}
	return count > 0, nil
}

// ActiveSecretGrants returns all active grants for a repository, keyed by
// (step, secret). The repository is identified by its durable ID so that
// same-name repos under different upstreams cannot alias each other's
// grants (#245 BLOCKER B).
func (s *Store) ActiveSecretGrants(ctx context.Context, repoID int64) (map[string]map[string]bool, error) {
	qualifiedName, err := s.QualifiedRepoName(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("resolve qualified repo name for grants: %w", err)
	}
	// Query with the qualified name ONLY. A bare-name fallback here would
	// match every same-name repository across upstreams — the exact grant
	// aliasing this ID-keyed lookup exists to prevent. There is no
	// transition window that needs one: the v12 migration qualifies every
	// persisted row that maps to a registered repository, and the access
	// reconciler canonicalizes ConfigMap entries before writing. A stray
	// bare row (only possible for a repo that was unregistered at both of
	// those boundaries) is deliberately inert until its grant is re-issued
	// against the registered identity.
	rows, err := s.db.QueryContext(ctx, `
SELECT step, secret FROM secret_access
WHERE repo = ? AND revoked_at IS NULL`, qualifiedName)
	if err != nil {
		return nil, fmt.Errorf("list active secret grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	grants := make(map[string]map[string]bool)
	for rows.Next() {
		var step, secret string
		if err := rows.Scan(&step, &secret); err != nil {
			return nil, fmt.Errorf("scan active secret grant: %w", err)
		}
		if grants[step] == nil {
			grants[step] = make(map[string]bool)
		}
		grants[step][secret] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active secret grants: %w", err)
	}
	return grants, nil
}

func scanSecretAccess(row rowScanner) (SecretAccessGrant, error) {
	var grant SecretAccessGrant
	var approvedAt int64
	var revokedAt sql.NullInt64
	if err := row.Scan(&grant.ID, &grant.Repo, &grant.Step, &grant.Secret,
		&grant.ApprovedBy, &approvedAt, &grant.RevokedBy, &revokedAt); err != nil {
		return SecretAccessGrant{}, err
	}
	grant.ApprovedAt = fromUnixNano(approvedAt)
	grant.RevokedAt = nullableTime(revokedAt)
	return grant, nil
}
