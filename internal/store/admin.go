package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

// UplinkStatus pairs one uplink with the usage state of its bound token
// credential for administrative listings. LastUsedAt is zero when the bearer
// token has never authenticated.
type UplinkStatus struct {
	model.Uplink
	LastUsedAt time.Time
}

// RegisterUpstream atomically creates operator configuration and its audit
// record. Administrative commands must not leave unaudited live mutations.
func (s *Store) RegisterUpstream(ctx context.Context, actor string, spec model.UpstreamSpec) (model.Upstream, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(spec.Name) == "" || strings.TrimSpace(spec.Kind) == "" || strings.TrimSpace(spec.BaseURL) == "" {
		return model.Upstream{}, fmt.Errorf("%w: actor and upstream fields are required", ErrInvalid)
	}
	// Guard 1: upstream names must match the repo segment charset and must not
	// be reserved names that would alias security boundaries (issue #245).
	if err := gitcache.ValidateUpstreamName(spec.Name); err != nil {
		return model.Upstream{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	details, err := json.Marshal(map[string]string{"name": spec.Name, "kind": spec.Kind, "base_url": spec.BaseURL, "key_name": spec.KeyName})
	if err != nil {
		return model.Upstream{}, fmt.Errorf("encode upstream audit: %w", err)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Upstream{}, fmt.Errorf("begin upstream registration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Org-scoped secret paths (oberth/upstream/<org>/*) and org-qualified SSH
	// path resolution both derive <org> from the trailing path segment of the
	// base URL (model.Upstream.Org). Two upstreams whose base URLs share that
	// segment map to the SAME secret subtree and SSH org, silently merging two
	// trust domains: a repository of upstream B becomes structurally eligible
	// for secrets seeded for upstream A. The name UNIQUE constraint does not
	// catch this because the names differ and the base URLs may differ by host
	// or scheme (github.com/acme vs gitlab.com/acme, or https:// vs ssh://).
	// Reject a colliding or empty Org() at admission, naming the conflict,
	// before the upstream exists (issue #217). This is the narrow admission
	// guard; scoping the virtual path by full upstream identity is the broader
	// Breaking change tracked separately.
	newOrg := model.Upstream{BaseURL: spec.BaseURL}.Org()
	if newOrg == "" {
		return model.Upstream{}, fmt.Errorf("%w: base URL %q yields an empty organization identity; org-scoped secret paths require a non-empty trailing path segment", ErrInvalid, spec.BaseURL)
	}
	existing, err := upstreamOrgConflict(ctx, tx, newOrg)
	if err != nil {
		return model.Upstream{}, err
	}
	if existing != "" {
		return model.Upstream{}, fmt.Errorf("%w: organization %q is already claimed by upstream %q; two upstreams with the same org would alias org-scoped secret paths (oberth/upstream/%s/*)", ErrInvalid, newOrg, existing, newOrg)
	}
	// Guard 2: namespace disjointness — upstream name must not collide with
	// any known upstream's org identity, and vice versa (issue #245). An
	// upstream named "oberthci" whose registered org is also "oberthci"
	// would make a 2-segment path like "oberthci/oberth" ambiguous: is it
	// org/repo or upstream/repo?
	orgConflictWithName, err := upstreamNameCollidesWithOrg(ctx, tx, spec.Name)
	if err != nil {
		return model.Upstream{}, err
	}
	if orgConflictWithName != "" {
		return model.Upstream{}, fmt.Errorf("%w: upstream name %q collides with the org identity of upstream %q; this would make 2-segment paths ambiguous", ErrInvalid, spec.Name, orgConflictWithName)
	}
	// Guard 3 (G2 reverse): the new upstream's org must not collide with
	// any existing upstream's NAME. Without this, adding upstream "acme"
	// with org "codeberg" when an upstream named "codeberg" already exists
	// would make "codeberg/repo" resolve ambiguously: is "codeberg" the
	// upstream name or the org identity? (issue #245 G2)
	nameConflictWithOrg, err := upstreamOrgCollidesWithName(ctx, tx, newOrg)
	if err != nil {
		return model.Upstream{}, err
	}
	if nameConflictWithOrg != "" {
		return model.Upstream{}, fmt.Errorf("%w: organization %q from base URL collides with existing upstream name %q; this would make 2-segment paths ambiguous", ErrInvalid, newOrg, nameConflictWithOrg)
	}

	result, err := tx.ExecContext(ctx, `
INSERT INTO upstreams(name, kind, base_url, key_name, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?)`,
		spec.Name, spec.Kind, spec.BaseURL, spec.KeyName, now, now)
	if err != nil {
		return model.Upstream{}, fmt.Errorf("register upstream: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.Upstream{}, fmt.Errorf("read upstream id: %w", err)
	}
	if _, err := appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: "upstream.register", ResourceType: "upstream",
		ResourceID: fmt.Sprint(id), Details: string(details),
	}, now); err != nil {
		return model.Upstream{}, fmt.Errorf("audit upstream registration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Upstream{}, fmt.Errorf("commit upstream registration: %w", err)
	}
	return s.Upstream(ctx, id)
}

// upstreamOrgConflict returns the name of an existing upstream whose Org()
// equals org, or "" when none collides. It fully drains and closes its cursor
// before returning so a subsequent write on the same transaction cannot block
// on an open read cursor (SQLite serializes reads and writes on one
// connection).
func upstreamOrgConflict(ctx context.Context, tx *sql.Tx, org string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name, base_url FROM upstreams`)
	if err != nil {
		return "", fmt.Errorf("check upstream org uniqueness: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var conflict string
	for rows.Next() {
		var name, baseURL string
		if err := rows.Scan(&name, &baseURL); err != nil {
			return "", fmt.Errorf("scan upstream org: %w", err)
		}
		if conflict == "" && (model.Upstream{BaseURL: baseURL}).Org() == org {
			conflict = name
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("check upstream org uniqueness: %w", err)
	}
	return conflict, nil
}

// upstreamNameCollidesWithOrg checks whether a proposed upstream name matches
// the Org() of any existing upstream (namespace disjointness guard #245).
// Returns the name of the conflicting upstream, or "" when no collision exists.
func upstreamNameCollidesWithOrg(ctx context.Context, tx *sql.Tx, upstreamName string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name, base_url FROM upstreams`)
	if err != nil {
		return "", fmt.Errorf("check upstream name/org disjointness: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var conflict string
	lowerName := strings.ToLower(upstreamName)
	for rows.Next() {
		var name, baseURL string
		if err := rows.Scan(&name, &baseURL); err != nil {
			return "", fmt.Errorf("scan upstream name/org: %w", err)
		}
		existingOrg := (model.Upstream{BaseURL: baseURL}).Org()
		// The proposed upstream NAME must not equal any existing upstream's ORG.
		if conflict == "" && strings.EqualFold(existingOrg, upstreamName) {
			conflict = name
		}
		// The reverse (existing NAME = new ORG) is checked by
		// upstreamOrgCollidesWithName in the caller.
		_ = lowerName
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("check upstream name/org disjointness: %w", err)
	}
	return conflict, nil
}

// upstreamOrgCollidesWithName checks whether the proposed upstream's org
// identity (from its base URL) matches the NAME of any existing upstream
// (G2 reverse direction, issue #245). Returns the name of the conflicting
// upstream, or "" when no collision exists.
func upstreamOrgCollidesWithName(ctx context.Context, tx *sql.Tx, org string) (string, error) {
	var conflict string
	err := tx.QueryRowContext(ctx, `SELECT name FROM upstreams WHERE LOWER(name) = LOWER(?)`, org).Scan(&conflict)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("check upstream org/name reverse disjointness: %w", err)
	}
	return conflict, nil
}

// RegisterRepository atomically maps one repository name to a configured
// upstream and records the audit action. Push-time discovery creates this
// mapping automatically only while exactly one upstream is configured; on a
// multi-upstream deployment an administrator declares the mapping explicitly
// before the repository's first push.
func (s *Store) RegisterRepository(ctx context.Context, actor string, spec model.RepositorySpec) (model.Repository, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(spec.Name) == "" || spec.UpstreamID <= 0 || strings.TrimSpace(spec.DefaultBranch) == "" {
		return model.Repository{}, fmt.Errorf("%w: actor and repository fields are required", ErrInvalid)
	}
	details, err := json.Marshal(map[string]string{
		"name": spec.Name, "upstream_id": fmt.Sprint(spec.UpstreamID), "default_branch": spec.DefaultBranch,
	})
	if err != nil {
		return model.Repository{}, fmt.Errorf("encode repository audit: %w", err)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Repository{}, fmt.Errorf("begin repository registration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
INSERT INTO repositories(name, upstream_id, default_branch, created_at, updated_at) VALUES(?, ?, ?, ?, ?)`,
		spec.Name, spec.UpstreamID, spec.DefaultBranch, now, now)
	if err != nil {
		return model.Repository{}, fmt.Errorf("register repository: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.Repository{}, fmt.Errorf("read repository id: %w", err)
	}
	if _, err := appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: "repository.register", ResourceType: "repository",
		ResourceID: fmt.Sprint(id), Details: string(details),
	}, now); err != nil {
		return model.Repository{}, fmt.Errorf("audit repository registration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Repository{}, fmt.Errorf("commit repository registration: %w", err)
	}
	return s.Repository(ctx, id)
}

// RemoveUpstream atomically removes one upstream and its mapped repositories,
// with the audit record, and returns the removed repository names. A mapped
// repository that already holds CI history (runs, promotions, receive events,
// publications, or issues reference it) fails the removal closed: history rows
// are immutable evidence and are never cascaded away.
func (s *Store) RemoveUpstream(ctx context.Context, actor, name string) ([]string, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: actor and upstream name are required", ErrInvalid)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin upstream removal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var upstreamID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM upstreams WHERE name = ?`, name).Scan(&upstreamID); err != nil {
		return nil, translateNotFound("upstream", err)
	}
	repositories, err := listUpstreamRepositoryNames(ctx, tx, upstreamID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM repositories WHERE upstream_id = ?`, upstreamID); err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return nil, fmt.Errorf("upstream %s has repositories with immutable CI history; removal is not supported once runs exist", name)
		}
		return nil, fmt.Errorf("remove upstream repositories: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM upstreams WHERE id = ?`, upstreamID); err != nil {
		return nil, fmt.Errorf("remove upstream: %w", err)
	}
	details, err := json.Marshal(map[string]string{"name": name, "removed_repositories": strings.Join(repositories, ",")})
	if err != nil {
		return nil, fmt.Errorf("encode upstream removal audit: %w", err)
	}
	if _, err := appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: "upstream.remove", ResourceType: "upstream",
		ResourceID: fmt.Sprint(upstreamID), Details: string(details),
	}, now); err != nil {
		return nil, fmt.Errorf("audit upstream removal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit upstream removal: %w", err)
	}
	return repositories, nil
}

// RemoveRepository atomically removes one repository mapping after verifying
// no runs are in-flight and no promotions are pending. The upstream name is
// resolved for the confirmation message. A repository that holds immutable CI
// history (runs, promotions, receive events, publications, or issues) fails
// the removal closed: history rows are immutable evidence and are never
// cascaded away. Deletion is audited. No tombstone is left, so upstream
// discovery can re-register the name on a future push.
func (s *Store) RemoveRepository(ctx context.Context, actor, name string) (RemovedRepository, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(name) == "" {
		return RemovedRepository{}, fmt.Errorf("%w: actor and repository name are required", ErrInvalid)
	}
	// Resolve the selector (bare, org/repo, or upstream/org/repo) through the
	// shared resolution rules FIRST: a bare name that exists under multiple
	// upstreams returns ErrAmbiguous instead of deleting an arbitrary row
	// (issue #264).
	selected, err := s.RepositoryByName(ctx, name)
	if err != nil {
		return RemovedRepository{}, err
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RemovedRepository{}, fmt.Errorf("begin repository removal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Re-read the authoritative row by id inside the transaction.
	var repoID, upstreamID int64
	var repoName, defaultBranch string
	var created, updated int64
	if err := tx.QueryRowContext(ctx, `
SELECT id, name, upstream_id, default_branch, created_at, updated_at
FROM repositories WHERE id = ?`, selected.ID).Scan(
		&repoID, &repoName, &upstreamID, &defaultBranch, &created, &updated); err != nil {
		return RemovedRepository{}, translateNotFound("repository", err)
	}
	var upstreamName, upstreamBaseURL string
	if err := tx.QueryRowContext(ctx, `SELECT name, base_url FROM upstreams WHERE id = ?`, upstreamID).Scan(&upstreamName, &upstreamBaseURL); err != nil {
		return RemovedRepository{}, fmt.Errorf("resolve upstream name: %w", err)
	}

	// Safety: refuse if any run is queued or running.
	var activeRuns int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM runs WHERE repo_id = ? AND status IN ('queued', 'running')`,
		repoID).Scan(&activeRuns); err != nil {
		return RemovedRepository{}, fmt.Errorf("check in-flight runs: %w", err)
	}
	if activeRuns > 0 {
		return RemovedRepository{}, fmt.Errorf("%w: repository %s has %d in-flight run(s); wait for them to finish before removing",
			ErrInvalidState, name, activeRuns)
	}

	// Safety: refuse if any promotion is pending.
	var pendingPromotions int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM promotions WHERE repo_id = ? AND status = 'pending'`,
		repoID).Scan(&pendingPromotions); err != nil {
		return RemovedRepository{}, fmt.Errorf("check pending promotions: %w", err)
	}
	if pendingPromotions > 0 {
		return RemovedRepository{}, fmt.Errorf("%w: repository %s has %d pending promotion(s); wait for them to finish before removing",
			ErrInvalidState, name, pendingPromotions)
	}

	// Attempt deletion. FK constraints from runs, promotions, receive_events,
	// publications, issues, ci_issue_work, ci_issue_projections, trusted_plans,
	// and trusted_applies will fail the deletion if any immutable history exists.
	if _, err := tx.ExecContext(ctx, `DELETE FROM repositories WHERE id = ?`, repoID); err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return RemovedRepository{}, fmt.Errorf("%w: repository %s has immutable CI history; removal is not supported once runs exist", ErrInvalidState, name)
		}
		return RemovedRepository{}, fmt.Errorf("remove repository: %w", err)
	}
	details, err := json.Marshal(map[string]string{
		"name":     name,
		"upstream": upstreamName,
	})
	if err != nil {
		return RemovedRepository{}, fmt.Errorf("encode repository removal audit: %w", err)
	}
	if _, err := appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: "repository.remove", ResourceType: "repository",
		ResourceID: fmt.Sprint(repoID), Details: string(details),
	}, now); err != nil {
		return RemovedRepository{}, fmt.Errorf("audit repository removal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RemovedRepository{}, fmt.Errorf("commit repository removal: %w", err)
	}
	return RemovedRepository{
		Repository: model.Repository{
			ID: repoID, Name: repoName, UpstreamID: upstreamID,
			DefaultBranch: defaultBranch,
			CreatedAt:     fromUnixNano(created), UpdatedAt: fromUnixNano(updated),
		},
		UpstreamName: upstreamName,
		UpstreamOrg:  model.Upstream{BaseURL: upstreamBaseURL}.Org(),
	}, nil
}

// RemovedRepository carries the removed repository's identity and its upstream
// name for the confirmation message. The Repository value is the state at
// removal time.
type RemovedRepository struct {
	model.Repository
	UpstreamName string
	// UpstreamOrg is the upstream's org identity at removal time, used by the
	// admin CLI to locate the org-qualified cache directory (issue #264).
	UpstreamOrg string
}

func listUpstreamRepositoryNames(ctx context.Context, tx *sql.Tx, upstreamID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM repositories WHERE upstream_id = ? ORDER BY name`, upstreamID)
	if err != nil {
		return nil, fmt.Errorf("list upstream repositories: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var repositories []string
	for rows.Next() {
		var repositoryName string
		if err := rows.Scan(&repositoryName); err != nil {
			return nil, fmt.Errorf("scan upstream repository: %w", err)
		}
		repositories = append(repositories, repositoryName)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list upstream repositories: %w", err)
	}
	return repositories, nil
}

func matchUplinksByIdentity(ctx context.Context, tx *sql.Tx, identity, fingerprint string) ([]model.Uplink, error) {
	query := `SELECT id, fingerprint, identity, token_credential_id, auth_actor, admin, created_at, updated_at FROM uplinks WHERE identity = ?`
	arguments := []any{identity}
	if strings.TrimSpace(fingerprint) != "" {
		query += ` AND fingerprint = ?`
		arguments = append(arguments, fingerprint)
	}
	rows, err := tx.QueryContext(ctx, query+` ORDER BY fingerprint`, arguments...)
	if err != nil {
		return nil, fmt.Errorf("look up uplink: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var matches []model.Uplink
	for rows.Next() {
		var value model.Uplink
		var created, updated int64
		var admin int
		if err := rows.Scan(&value.ID, &value.Fingerprint, &value.Identity, &value.TokenCredentialID,
			&value.AuthActor, &admin, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan uplink: %w", err)
		}
		value.Admin = admin != 0
		value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
		matches = append(matches, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("look up uplink: %w", err)
	}
	return matches, nil
}

// ListUplinkStatuses returns every registered uplink with the usage state of
// its bound token credential, ordered by identity for stable listings.
func (s *Store) ListUplinkStatuses(ctx context.Context) ([]UplinkStatus, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT u.id, u.fingerprint, u.identity, u.token_credential_id, u.auth_actor, u.admin, u.created_at, u.updated_at,
       COALESCE(t.last_used_at, 0)
FROM uplinks u LEFT JOIN token_credentials t ON t.id = u.token_credential_id
ORDER BY u.identity, u.fingerprint`)
	if err != nil {
		return nil, fmt.Errorf("list uplinks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var values []UplinkStatus
	for rows.Next() {
		var value UplinkStatus
		var created, updated, lastUsed int64
		var admin int
		if err := rows.Scan(&value.ID, &value.Fingerprint, &value.Identity, &value.TokenCredentialID,
			&value.AuthActor, &admin, &created, &updated, &lastUsed); err != nil {
			return nil, fmt.Errorf("scan uplink: %w", err)
		}
		value.Admin = admin != 0
		value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
		if lastUsed > 0 {
			value.LastUsedAt = fromUnixNano(lastUsed)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list uplinks: %w", err)
	}
	return values, nil
}

// RemoveUplink atomically deletes one uplink, revokes its bound token
// credential so the bearer token is immediately invalid, and records the audit
// action. fingerprint is optional and disambiguates when several uplinks share
// an identity.
func (s *Store) RemoveUplink(ctx context.Context, actor, identity, fingerprint string) (model.Uplink, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(identity) == "" {
		return model.Uplink{}, fmt.Errorf("%w: actor and uplink identity are required", ErrInvalid)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Uplink{}, fmt.Errorf("begin uplink removal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	matches, err := matchUplinksByIdentity(ctx, tx, identity, fingerprint)
	if err != nil {
		return model.Uplink{}, err
	}
	if len(matches) == 0 {
		return model.Uplink{}, fmt.Errorf("%w: uplink %s", ErrNotFound, identity)
	}
	if len(matches) > 1 {
		fingerprints := make([]string, 0, len(matches))
		for _, match := range matches {
			fingerprints = append(fingerprints, match.Fingerprint)
		}
		return model.Uplink{}, fmt.Errorf("%w: identity %s has %d uplinks (%s); pass --fingerprint",
			ErrAmbiguous, identity, len(matches), strings.Join(fingerprints, ", "))
	}
	value := matches[0]
	if _, err := tx.ExecContext(ctx, `DELETE FROM uplinks WHERE id = ?`, value.ID); err != nil {
		return model.Uplink{}, fmt.Errorf("remove uplink: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE token_credentials SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now, value.TokenCredentialID); err != nil {
		return model.Uplink{}, fmt.Errorf("revoke uplink token credential: %w", err)
	}
	details, err := json.Marshal(map[string]string{"identity": value.Identity, "fingerprint": value.Fingerprint})
	if err != nil {
		return model.Uplink{}, fmt.Errorf("encode uplink removal audit: %w", err)
	}
	if _, err := appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: "uplink.remove", ResourceType: "uplink",
		ResourceID: fmt.Sprint(value.ID), Details: string(details),
	}, now); err != nil {
		return model.Uplink{}, fmt.Errorf("audit uplink removal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Uplink{}, fmt.Errorf("commit uplink removal: %w", err)
	}
	return value, nil
}

// RegisterUplink atomically binds one pending token credential to a public-key
// identity, revokes any displaced credential, and records who performed the
// registration. Token activation occurs only after this method commits.
func (s *Store) RegisterUplink(ctx context.Context, actor string, spec model.UplinkSpec) (model.Uplink, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(spec.Fingerprint) == "" || strings.TrimSpace(spec.Identity) == "" ||
		strings.TrimSpace(spec.TokenCredentialID) == "" {
		return model.Uplink{}, fmt.Errorf("%w: actor and uplink fields are required", ErrInvalid)
	}
	adminInt := 0
	if spec.Admin {
		adminInt = 1
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Uplink{}, fmt.Errorf("begin uplink registration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var displacedCredentialID string
	_ = tx.QueryRowContext(ctx, `SELECT token_credential_id FROM uplinks WHERE fingerprint = ?`,
		spec.Fingerprint).Scan(&displacedCredentialID)
	row := tx.QueryRowContext(ctx, `
INSERT INTO uplinks(fingerprint, identity, token_credential_id, auth_actor, admin, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(fingerprint) DO UPDATE SET
    identity = excluded.identity,
    token_credential_id = excluded.token_credential_id,
    auth_actor = excluded.auth_actor,
    admin = excluded.admin,
    updated_at = excluded.updated_at
RETURNING id, fingerprint, identity, token_credential_id, auth_actor, admin, created_at, updated_at`,
		spec.Fingerprint, spec.Identity, spec.TokenCredentialID, actor, adminInt, now, now)
	var value model.Uplink
	var created, updated int64
	var scannedAdmin int
	if err := row.Scan(&value.ID, &value.Fingerprint, &value.Identity, &value.TokenCredentialID, &value.AuthActor, &scannedAdmin, &created, &updated); err != nil {
		return model.Uplink{}, fmt.Errorf("register uplink: %w", err)
	}
	value.Admin = scannedAdmin != 0
	if displacedCredentialID != "" && displacedCredentialID != spec.TokenCredentialID {
		if _, err := tx.ExecContext(ctx, `
UPDATE token_credentials SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now, displacedCredentialID); err != nil {
			return model.Uplink{}, fmt.Errorf("revoke displaced credential: %w", err)
		}
	}
	auditDetails := map[string]any{
		"fingerprint": spec.Fingerprint,
		"identity":    spec.Identity,
		"admin":       spec.Admin,
	}
	if displacedCredentialID != "" && displacedCredentialID != spec.TokenCredentialID {
		auditDetails["displaced_credential"] = displacedCredentialID
	}
	details, err := json.Marshal(auditDetails)
	if err != nil {
		return model.Uplink{}, fmt.Errorf("encode uplink audit: %w", err)
	}
	if _, err := appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: "uplink.register", ResourceType: "uplink",
		ResourceID: fmt.Sprint(value.ID), Details: string(details),
	}, now); err != nil {
		return model.Uplink{}, fmt.Errorf("audit uplink registration: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Uplink{}, fmt.Errorf("commit uplink registration: %w", err)
	}
	value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return value, nil
}
