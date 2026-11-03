package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type ResourceGrant struct {
	RepoID    int64                       `json:"repo_id"`
	Resources corev1.ResourceRequirements `json:"resources"`
	GrantedBy string                      `json:"granted_by"`
	GrantedAt time.Time                   `json:"granted_at"`
	RevokedBy string                      `json:"revoked_by,omitempty"`
	RevokedAt *time.Time                  `json:"revoked_at,omitempty"`
}

func DefaultResourceCeiling() corev1.ResourceRequirements {
	c, _ := argoworkflow.ResourceCeilingForSize(periapsis.L)
	return argoworkflow.ResourceRequirementsForCeiling(c)
}

func (s *Store) ResourceGrant(ctx context.Context, repoID int64) (ResourceGrant, error) {
	var g ResourceGrant
	var raw string
	var at int64
	var revoked sql.NullInt64
	err := s.reader().QueryRowContext(ctx, `SELECT repo_id,resources_json,granted_by,granted_at,revoked_by,revoked_at FROM resource_ceilings WHERE repo_id=?`, repoID).Scan(&g.RepoID, &raw, &g.GrantedBy, &at, &g.RevokedBy, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return g, fmt.Errorf("%w: resource grant", ErrNotFound)
	}
	if err != nil {
		return g, err
	}
	if err = json.Unmarshal([]byte(raw), &g.Resources); err != nil {
		return g, err
	}
	if err = argoworkflow.ValidateAdministratorResources(g.Resources); err != nil {
		return g, err
	}
	g.GrantedAt = fromUnixNano(at)
	if revoked.Valid {
		t := fromUnixNano(revoked.Int64)
		g.RevokedAt = &t
	}
	return g, nil
}

func (s *Store) ResourceCeiling(ctx context.Context, repoID int64) (corev1.ResourceRequirements, error) {
	if repoID <= 0 {
		return corev1.ResourceRequirements{}, fmt.Errorf("%w: repository ID is required", ErrInvalid)
	}
	g, err := s.ResourceGrant(ctx, repoID)
	if errors.Is(err, ErrNotFound) || err == nil && g.RevokedAt != nil {
		return DefaultResourceCeiling(), nil
	}
	return g.Resources, err
}

// SetResourceCeiling audits the numeric ceiling in the same transaction. Nil
// revokes to the default. Legacy size/priority policy is independent after upgrade.
func (s *Store) SetResourceCeiling(ctx context.Context, repoID int64, actor string, ceiling *corev1.ResourceRequirements) (ResourceGrant, error) {
	if repoID <= 0 || strings.TrimSpace(actor) == "" {
		return ResourceGrant{}, fmt.Errorf("%w: repository and actor required", ErrInvalid)
	}
	resources := DefaultResourceCeiling()
	if ceiling != nil {
		resources = *ceiling.DeepCopy()
	}
	if err := argoworkflow.ValidateAdministratorResources(resources); err != nil {
		return ResourceGrant{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	raw, err := json.Marshal(resources)
	if err != nil {
		return ResourceGrant{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ResourceGrant{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM repositories WHERE id=?)`, repoID).Scan(&exists); err != nil {
		return ResourceGrant{}, err
	}
	if !exists {
		return ResourceGrant{}, fmt.Errorf("%w: repository", ErrNotFound)
	}
	if ceiling != nil {
		if err = requireRepositoryActive(ctx, tx, repoID); err != nil {
			return ResourceGrant{}, err
		}
	}
	var previous string
	var revoked sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT resources_json,revoked_at FROM resource_ceilings WHERE repo_id=?`, repoID).Scan(&previous, &revoked)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ResourceGrant{}, err
	}
	if ceiling == nil && revoked.Valid {
		return ResourceGrant{}, fmt.Errorf("%w: resource grant already revoked", ErrInvalidState)
	}
	now := unixNano(s.now())
	action := "resource.grant"
	if ceiling != nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO resource_ceilings(repo_id,resources_json,granted_by,granted_at) VALUES(?,?,?,?) ON CONFLICT(repo_id) DO UPDATE SET resources_json=excluded.resources_json,granted_by=excluded.granted_by,granted_at=excluded.granted_at,revoked_by='',revoked_at=NULL`, repoID, string(raw), actor, now)
	} else {
		action = "resource.revoke"
		_, err = tx.ExecContext(ctx, `INSERT INTO resource_ceilings(repo_id,resources_json,granted_by,granted_at,revoked_by,revoked_at) VALUES(?,?,?,?,?,?) ON CONFLICT(repo_id) DO UPDATE SET revoked_by=excluded.revoked_by,revoked_at=excluded.revoked_at`, repoID, string(raw), actor, now, actor, now)
	}
	if err != nil {
		return ResourceGrant{}, err
	}
	details, _ := json.Marshal(map[string]any{"repo_id": repoID, "resources": resources, "previous_resources_json": previous})
	if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: action, ResourceType: "repository", ResourceID: fmt.Sprint(repoID), Details: string(details)}, now); err != nil {
		return ResourceGrant{}, err
	}
	if err = tx.Commit(); err != nil {
		return ResourceGrant{}, err
	}
	return s.ResourceGrant(ctx, repoID)
}

// ParseResourceCeiling accepts Kubernetes quantities, with the existing storage
// ceiling when omitted. CPU and memory must be explicit in both tiers.
func ParseResourceCeiling(requests, limits map[string]string) (corev1.ResourceRequirements, error) {
	out := corev1.ResourceRequirements{}
	parse := func(values map[string]string) (corev1.ResourceList, error) {
		result := corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("16Gi")}
		for name, raw := range values {
			q, err := resource.ParseQuantity(raw)
			if err != nil {
				return nil, err
			}
			result[corev1.ResourceName(name)] = q
		}
		return result, nil
	}
	var err error
	out.Requests, err = parse(requests)
	if err != nil {
		return out, err
	}
	out.Limits, err = parse(limits)
	if err != nil {
		return out, err
	}
	return out, argoworkflow.ValidateAdministratorResources(out)
}
