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

// RepositoryUpstreamNames maps every registered repository ID to the name of
// its upstream, which uplink scopes match against.
func (s *Store) RepositoryUpstreamNames(ctx context.Context) (map[int64]string, error) {
	rows, err := s.reader().QueryContext(ctx, `SELECT r.id, u.name FROM repositories r JOIN upstreams u ON u.id = r.upstream_id`)
	if err != nil {
		return nil, fmt.Errorf("list repository upstreams: %w", err)
	}
	defer func() { _ = rows.Close() }()
	names := map[int64]string{}
	for rows.Next() {
		var id int64
		var upstream string
		if err := rows.Scan(&id, &upstream); err != nil {
			return nil, fmt.Errorf("scan repository upstream: %w", err)
		}
		names[id] = upstream
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list repository upstreams: %w", err)
	}
	return names, nil
}

// ConsumePromotionGrant marks a grant as consumed by a specific promotion.
func (s *Store) ConsumePromotionGrant(ctx context.Context, grantID, promotionID, actor string) error {
	if strings.TrimSpace(grantID) == "" || strings.TrimSpace(promotionID) == "" {
		return fmt.Errorf("%w: grant ID and promotion ID are required", ErrInvalid)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin consume promotion grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
UPDATE promotion_grants SET consumed_by = ?, consumed_at = ?
WHERE id = ? AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
		promotionID, now, grantID, now)
	if err != nil {
		return fmt.Errorf("consume promotion grant: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("consume promotion grant rows: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("%w: promotion grant is not active", ErrInvalidState)
	}
	details, _ := json.Marshal(map[string]string{
		"promotion_id": promotionID,
	})
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: "promotion_grant.consume", ResourceType: "promotion_grant",
		ResourceID: grantID, Details: string(details),
	}, now); err != nil {
		return fmt.Errorf("audit promotion grant consumption: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit consume promotion grant: %w", err)
	}
	return nil
}

func (s *Store) AuditAnchors(ctx context.Context) ([]model.AuditAnchor, error) {
	rows, err := s.auditReader().QueryContext(ctx, `
SELECT id, audit_id, audit_sha256, tsa_url, receipt, anchored_at, created_at
FROM audit_anchors ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list audit anchors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var anchors []model.AuditAnchor
	for rows.Next() {
		anchor, err := scanAuditAnchor(rows)
		if err != nil {
			return nil, fmt.Errorf("scan audit anchor: %w", err)
		}
		anchors = append(anchors, anchor)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit anchors: %w", err)
	}
	return anchors, nil
}

func (s *Store) VerifyAuditAnchorReference(ctx context.Context, anchor model.AuditAnchor) error {
	return s.VerifyAuditAnchorReferences(ctx, []model.AuditAnchor{anchor})
}

// SecretAccessCheck reports whether an active (non-revoked) grant exists for
// the given (repo, step, secret) triple.
func (s *Store) SecretAccessCheck(ctx context.Context, repo, step, secret string) (bool, error) {
	if strings.TrimSpace(repo) == "" || strings.TrimSpace(step) == "" || strings.TrimSpace(secret) == "" {
		return false, fmt.Errorf("%w: repo, step, and secret are required", ErrInvalid)
	}
	var count int
	if err := s.reader().QueryRowContext(ctx, `
SELECT count(*) FROM secret_access
WHERE repo = ? AND step = ? AND secret = ? AND revoked_at IS NULL`,
		repo, step, secret).Scan(&count); err != nil {
		return false, fmt.Errorf("check secret access: %w", err)
	}
	return count > 0, nil
}

// UpsertCIIssue creates or updates a CI issue for a red branch. Production
// callers go through the issue projection system; this method is retained for
// test fixtures that need a CI issue without a full run lifecycle.
func (s *Store) UpsertCIIssue(ctx context.Context, actor string, repoID int64, branch, title, body string) (model.Issue, error) {
	if strings.TrimSpace(actor) == "" || repoID <= 0 || strings.TrimSpace(branch) == "" || strings.TrimSpace(title) == "" {
		return model.Issue{}, fmt.Errorf("%w: CI issue fields are invalid", ErrInvalid)
	}
	acquired := s.now().UTC()
	now := unixNano(acquired)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Issue{}, fmt.Errorf("begin upsert CI issue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	value, err := scanIssue(tx.QueryRowContext(ctx, `
INSERT INTO issues(repo_id, kind, branch, title, body, state, occurrences, created_at, updated_at)
VALUES(?, 'ci', ?, ?, ?, 'open', 1, ?, ?)
ON CONFLICT(repo_id, branch) WHERE kind = 'ci' AND state = 'open' DO UPDATE SET
    title = excluded.title,
    occurrences = issues.occurrences + 1,
    updated_at = excluded.updated_at
RETURNING `+issueColumns, repoID, branch, title, body, now, now))
	if err != nil {
		return model.Issue{}, fmt.Errorf("upsert CI issue: %w", err)
	}
	if value.Occurrences > 1 {
		if err := s.addIssueComment(ctx, tx, actor, value.ID, "evidence", body, acquired); err != nil {
			return model.Issue{}, err
		}
	}
	if err := s.appendIssueAudit(ctx, tx, actor, "issue.ci.upsert", value.ID, map[string]any{"branch": branch, "occurrences": value.Occurrences}, now); err != nil {
		return model.Issue{}, err
	}
	expires := acquired.Add(IssueLockTTL)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO issue_locks(issue_id, owner, acquired_at, expires_at) VALUES(?, ?, ?, ?)
ON CONFLICT(issue_id) DO UPDATE SET
    owner = excluded.owner,
    acquired_at = excluded.acquired_at,
    expires_at = excluded.expires_at`, value.ID, actor, now, unixNano(expires)); err != nil {
		return model.Issue{}, fmt.Errorf("lock CI issue for pushing uplink: %w", err)
	}
	if err := s.appendIssueAudit(ctx, tx, actor, "issue.lock.acquire", value.ID, map[string]any{"expires_at": expires.Format(time.RFC3339Nano), "source": "ci_red"}, now); err != nil {
		return model.Issue{}, err
	}
	value, err = scanIssue(tx.QueryRowContext(ctx, `SELECT `+issueColumns+` FROM issues WHERE id=?`, value.ID))
	if err != nil {
		return value, err
	}
	if err := tx.Commit(); err != nil {
		return model.Issue{}, fmt.Errorf("commit upsert CI issue: %w", err)
	}
	return value, nil
}

// CreateUpstream inserts a named upstream. Production callers go through the
// discovery-based bootstrap path; this method is retained for test fixtures
// where fabricating a discovery response is unnecessary overhead.
func (s *Store) CreateUpstream(ctx context.Context, spec model.UpstreamSpec) (model.Upstream, error) {
	if strings.TrimSpace(spec.Name) == "" || strings.TrimSpace(spec.Kind) == "" || strings.TrimSpace(spec.BaseURL) == "" {
		return model.Upstream{}, fmt.Errorf("%w: upstream fields are required", ErrInvalid)
	}
	now := unixNano(s.now())
	result, err := s.db.ExecContext(ctx, `
INSERT INTO upstreams(name, kind, base_url, key_name, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?)`,
		spec.Name, spec.Kind, spec.BaseURL, spec.KeyName, now, now)
	if err != nil {
		return model.Upstream{}, fmt.Errorf("create upstream: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.Upstream{}, fmt.Errorf("read upstream id: %w", err)
	}
	return s.Upstream(ctx, id)
}

// EnqueuePromotionRun commits the merged-tree run and its immutable promotion
// association together. Production callers go through the admitted plan/apply
// path; this method is retained for test fixtures that need a promotion-bound
// run without fabricating a full trusted plan flow.
func (s *Store) EnqueuePromotionRun(ctx context.Context, runSpec model.RunSpec, promotionSpec model.PromotionSpec) (model.EnqueueRunResult, model.Promotion, error) {
	validatedRun, err := validateRunSpec(runSpec)
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, err
	}
	if err := validatePromotionSpec(promotionSpec); err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, err
	}
	if validatedRun.Trigger != "promotion" || validatedRun.Release || validatedRun.RefKind != model.RefBranch ||
		validatedRun.RepoID != promotionSpec.RepoID || validatedRun.Actor != promotionSpec.Actor ||
		!strings.EqualFold(validatedRun.SHA, promotionSpec.ResultSHA) ||
		!strings.EqualFold(validatedRun.TestedSHA, promotionSpec.ResultSHA) ||
		!strings.EqualFold(validatedRun.BaseSHA, promotionSpec.PreviousSHA) {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("%w: promotion run does not match promotion", ErrInvalid)
	}
	runID, err := randomID()
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("generate promotion run id: %w", err)
	}
	promotionID, err := randomID()
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("generate promotion id: %w", err)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("begin promotion run enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	enqueued, err := s.enqueueRunTx(ctx, tx, validatedRun, runID, now, 0)
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, err
	}
	promotion, err := insertPromotion(ctx, tx, promotionSpec, promotionID, runID, promotionSpec.GrantID, now)
	if err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, err
	}
	if err := s.appendPromotionAdmissionAudit(ctx, tx, promotion, now); err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.EnqueueRunResult{}, model.Promotion{}, fmt.Errorf("commit promotion run enqueue: %w", err)
	}
	return enqueued, promotion, nil
}

// UpsertUplink inserts or updates an uplink record. Production callers go
// through the admin CLI gate which assembles the full admission chain; this
// method is retained for test fixtures that need an uplink without the CLI.
func (s *Store) UpsertUplink(ctx context.Context, spec model.UplinkSpec) (model.Uplink, error) {
	if strings.TrimSpace(spec.Fingerprint) == "" || strings.TrimSpace(spec.Identity) == "" ||
		strings.TrimSpace(spec.TokenCredentialID) == "" || strings.TrimSpace(spec.AuthActor) == "" {
		return model.Uplink{}, fmt.Errorf("%w: uplink fields are required", ErrInvalid)
	}
	adminInt := 0
	if spec.Admin {
		adminInt = 1
	}
	scopes, err := normalizeUplinkScopes(spec.Scope)
	if err != nil {
		return model.Uplink{}, err
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Uplink{}, fmt.Errorf("begin upsert uplink: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
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
		spec.Fingerprint, spec.Identity, spec.TokenCredentialID, spec.AuthActor, adminInt, now, now)
	var value model.Uplink
	var created, updated int64
	var scannedAdmin int
	if err := row.Scan(&value.ID, &value.Fingerprint, &value.Identity, &value.TokenCredentialID, &value.AuthActor, &scannedAdmin, &created, &updated); err != nil {
		return model.Uplink{}, fmt.Errorf("upsert uplink: %w", err)
	}
	value.Admin = scannedAdmin != 0
	if err := replaceUplinkScopes(ctx, tx, value.ID, scopes); err != nil {
		return model.Uplink{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Uplink{}, fmt.Errorf("commit upsert uplink: %w", err)
	}
	if len(scopes) > 0 {
		value.Scope = scopes
	}
	value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return value, nil
}

// TokenCredentialByDigest looks up a credential by its SHA-256 digest.
// Production callers use AuthenticatedUplinkByDigest which joins the uplink in
// one query; this method is retained for test fixtures that verify credential
// lifecycle state independently.
func (s *Store) TokenCredentialByDigest(ctx context.Context, digest []byte) (model.TokenCredential, error) {
	if len(digest) != 32 {
		return model.TokenCredential{}, ErrNotFound
	}
	release, err := acquireAuthSlot(ctx, s.bearerAuthSlots)
	if err != nil {
		return model.TokenCredential{}, fmt.Errorf("wait for bearer credential reader: %w", err)
	}
	defer release()
	value, err := scanTokenCredential(s.authReader().QueryRowContext(ctx, `
SELECT id, name, digest, created_at, activated_at, last_used_at, revoked_at, expires_at
FROM token_credentials WHERE digest = ?`, digest))
	if err != nil {
		return model.TokenCredential{}, translateNotFound("token credential", err)
	}
	return value, nil
}

func (s *Store) RevokeTokenCredential(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin revoke token credential: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	changed, err := revokeTokenCredentialTx(ctx, tx, id, unixNano(s.now()))
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("%w: token credential", ErrNotFound)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit revoke token credential: %w", err)
	}
	return nil
}

// EnqueueRun creates a standalone run without a durable receive event
// association. Production callers go through EnqueueReceiveEvent which binds
// the receive idempotency key atomically; this wrapper is retained for test
// setup where fabricating a receive event is unnecessary overhead.
func (s *Store) EnqueueRun(ctx context.Context, spec model.RunSpec) (model.EnqueueRunResult, error) {
	spec, err := validateRunSpec(spec)
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	id, err := randomID()
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("generate run id: %w", err)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("begin enqueue run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := s.enqueueRunTx(ctx, tx, spec, id, now, 0)
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("commit enqueue run: %w", err)
	}
	return result, nil
}

// ReleaseIssueLock explicitly releases a lock before expiry. Production callers
// rely on TTL expiry; this method is retained for test fixtures that need
// immediate lock release without waiting.
func (s *Store) ReleaseIssueLock(ctx context.Context, issueID int64, owner string) error {
	if issueID <= 0 || strings.TrimSpace(owner) == "" {
		return fmt.Errorf("%w: issue and lock owner are required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin release issue lock: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	value, err := scanIssueLock(tx.QueryRowContext(ctx, `
DELETE FROM issue_locks WHERE issue_id = ? AND owner = ?
RETURNING issue_id, owner, acquired_at, expires_at`, issueID, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLockNotOwned
	}
	if err != nil {
		return fmt.Errorf("release issue lock: %w", err)
	}
	if err := s.appendIssueAudit(ctx, tx, owner, "issue.lock.release", issueID, map[string]any{"acquired_at": value.AcquiredAt.Format(time.RFC3339Nano)}, unixNano(s.now())); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit release issue lock: %w", err)
	}
	return nil
}

func (s *Store) UpstreamByName(ctx context.Context, name string) (model.Upstream, error) {
	return scanUpstream(s.reader().QueryRowContext(ctx, `
SELECT id, name, kind, base_url, key_name, created_at, updated_at FROM upstreams WHERE name = ?`, name))
}
func (s *Store) LatestRunForRef(ctx context.Context, repoID int64, ref string) (model.Run, error) {
	return s.latestRunForRef(ctx, repoID, ref)
}
