package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// Upstream-scoped uplinks (#073): an uplink with scope rows sees and touches
// only the repositories of the upstreams its rows name.

// maximumUplinkScopes bounds one uplink's scope list.
const maximumUplinkScopes = 64

var scopeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type scopeQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// repoRestriction narrows a query to IDs: nil adds nothing, an empty slice
// matches no row. column must be a trusted SQL expression.
func repoRestriction(column string, ids []int64) (string, []any) {
	if ids == nil {
		return "", nil
	}
	if len(ids) == 0 {
		return " AND 0", nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for index, id := range ids {
		args[index] = id
	}
	return " AND " + column + " IN (" + placeholders + ")", args
}

// RepositoryUpstreamNames maps every registered repository ID to the name of
// its upstream, which uplink scopes match against.
func (s *Store) RepositoryUpstreamNames(ctx context.Context) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, u.name FROM repositories r JOIN upstreams u ON u.id = r.upstream_id`)
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

func normalizeUplinkScopes(scopes []string) ([]string, error) {
	if len(scopes) > maximumUplinkScopes {
		return nil, fmt.Errorf("%w: at most %d uplink scopes", ErrInvalid, maximumUplinkScopes)
	}
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if !scopeName.MatchString(scope) {
			return nil, fmt.Errorf("%w: uplink scope %q is not an upstream name", ErrInvalid, scope)
		}
		if !slices.Contains(out, scope) {
			out = append(out, scope)
		}
	}
	slices.Sort(out)
	return out, nil
}

// ResolveUplinkScopes checks that every scope names a registered upstream
// and returns them deduplicated and sorted.
func (s *Store) ResolveUplinkScopes(ctx context.Context, inputs []string) ([]string, error) {
	upstreams, err := s.ListUpstreams(ctx)
	if err != nil {
		return nil, err
	}
	known := make([]string, 0, len(upstreams))
	for _, upstream := range upstreams {
		known = append(known, upstream.Name)
	}
	resolved := make([]string, 0, len(inputs))
	for _, input := range inputs {
		name := strings.TrimSpace(input)
		if !slices.Contains(known, name) {
			return nil, fmt.Errorf("%w: scope %q: no upstream by that name; a scope is an upstream name (%s)",
				ErrInvalid, input, strings.Join(known, ", "))
		}
		resolved = append(resolved, name)
	}
	return normalizeUplinkScopes(resolved)
}

func loadUplinkScopes(ctx context.Context, querier scopeQuerier, uplinkID int64) ([]string, error) {
	rows, err := querier.QueryContext(ctx, `SELECT scope FROM uplink_scopes WHERE uplink_id = ? ORDER BY scope`, uplinkID)
	if err != nil {
		return nil, fmt.Errorf("read uplink scope: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var scopes []string
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return nil, fmt.Errorf("scan uplink scope: %w", err)
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read uplink scope: %w", err)
	}
	return scopes, nil
}

func loadAllUplinkScopes(ctx context.Context, querier scopeQuerier) (map[int64][]string, error) {
	rows, err := querier.QueryContext(ctx, `SELECT uplink_id, scope FROM uplink_scopes ORDER BY uplink_id, scope`)
	if err != nil {
		return nil, fmt.Errorf("read uplink scopes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	scopes := map[int64][]string{}
	for rows.Next() {
		var id int64
		var scope string
		if err := rows.Scan(&id, &scope); err != nil {
			return nil, fmt.Errorf("scan uplink scope: %w", err)
		}
		scopes[id] = append(scopes[id], scope)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read uplink scopes: %w", err)
	}
	return scopes, nil
}

func replaceUplinkScopes(ctx context.Context, tx *sql.Tx, uplinkID int64, scopes []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM uplink_scopes WHERE uplink_id = ?`, uplinkID); err != nil {
		return fmt.Errorf("clear uplink scope: %w", err)
	}
	for _, scope := range scopes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO uplink_scopes(uplink_id, scope) VALUES(?, ?)`, uplinkID, scope); err != nil {
			return fmt.Errorf("write uplink scope: %w", err)
		}
	}
	return nil
}

// SetUplinkScope replaces one uplink's scope and records the audit action.
// An empty scope makes the uplink unscoped again. fingerprint is optional and
// disambiguates when several uplinks share an identity. Scopes must already
// name registered upstreams (ResolveUplinkScopes).
func (s *Store) SetUplinkScope(ctx context.Context, actor, identity, fingerprint string, scopes []string) (model.Uplink, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(identity) == "" {
		return model.Uplink{}, fmt.Errorf("%w: actor and uplink identity are required", ErrInvalid)
	}
	normalized, err := normalizeUplinkScopes(scopes)
	if err != nil {
		return model.Uplink{}, err
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Uplink{}, fmt.Errorf("begin uplink scope change: %w", err)
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
	previous, err := loadUplinkScopes(ctx, tx, value.ID)
	if err != nil {
		return model.Uplink{}, err
	}
	if err := replaceUplinkScopes(ctx, tx, value.ID, normalized); err != nil {
		return model.Uplink{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE uplinks SET updated_at = ? WHERE id = ?`, now, value.ID); err != nil {
		return model.Uplink{}, fmt.Errorf("touch uplink: %w", err)
	}
	details, err := json.Marshal(map[string]any{
		"identity": value.Identity, "fingerprint": value.Fingerprint,
		"scope": scopeAuditValue(normalized), "previous_scope": scopeAuditValue(previous),
	})
	if err != nil {
		return model.Uplink{}, fmt.Errorf("encode uplink scope audit: %w", err)
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor: actor, Action: "uplink.scope", ResourceType: "uplink",
		ResourceID: fmt.Sprint(value.ID), Details: string(details),
	}, now); err != nil {
		return model.Uplink{}, fmt.Errorf("audit uplink scope change: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Uplink{}, fmt.Errorf("commit uplink scope change: %w", err)
	}
	value.Scope = normalized
	value.UpdatedAt = fromUnixNano(now)
	return value, nil
}

// scopeAuditValue records an unscoped uplink as an empty list, never null.
func scopeAuditValue(scopes []string) []string {
	if scopes == nil {
		return []string{}
	}
	return scopes
}
