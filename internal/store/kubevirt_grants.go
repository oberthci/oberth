package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/internal/model"
)

// KubeVirtGrant belongs to a durable repository ID, never an ambiguous name.
type KubeVirtGrant struct {
	RepoID int64 `json:"repo_id"`
	KubeVirtBudget
	GrantedBy string     `json:"granted_by"`
	GrantedAt time.Time  `json:"granted_at"`
	RevokedBy string     `json:"revoked_by,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// KubeVirtBudget is a grant's VMI budget (#1017). The zero value
// selects DefaultKubeVirtBudget. Admission applies the server ceiling.
type KubeVirtBudget struct {
	MaxVMs         int   `json:"max_vms"`
	MaxCores       int   `json:"max_cores"`
	MaxMemoryBytes int64 `json:"max_memory_bytes"`
}

// These bounds match the v25 CHECK constraints and argoworkflow.VMIBudgetLimit.
const (
	maxKubeVirtVMs         = 16
	maxKubeVirtCores       = 64
	minKubeVirtMemoryBytes = int64(1) << 30
	maxKubeVirtMemoryBytes = int64(256) << 30
)

// DefaultKubeVirtBudget is the budget of a grant without explicit values.
func DefaultKubeVirtBudget() KubeVirtBudget {
	return KubeVirtBudget{MaxVMs: 2, MaxCores: 6, MaxMemoryBytes: 12 << 30}
}

// WithDefaults fills each unset field from DefaultKubeVirtBudget.
func (budget KubeVirtBudget) WithDefaults() KubeVirtBudget {
	defaults := DefaultKubeVirtBudget()
	if budget.MaxVMs == 0 {
		budget.MaxVMs = defaults.MaxVMs
	}
	if budget.MaxCores == 0 {
		budget.MaxCores = defaults.MaxCores
	}
	if budget.MaxMemoryBytes == 0 {
		budget.MaxMemoryBytes = defaults.MaxMemoryBytes
	}
	return budget
}

// Validate checks a complete budget against the schema bounds.
func (budget KubeVirtBudget) Validate() error {
	if budget.MaxVMs < 1 || budget.MaxVMs > maxKubeVirtVMs {
		return fmt.Errorf("%w: KubeVirt max VMs must be 1..%d", ErrInvalid, maxKubeVirtVMs)
	}
	if budget.MaxCores < 1 || budget.MaxCores > maxKubeVirtCores {
		return fmt.Errorf("%w: KubeVirt max cores must be 1..%d", ErrInvalid, maxKubeVirtCores)
	}
	if budget.MaxMemoryBytes < minKubeVirtMemoryBytes || budget.MaxMemoryBytes > maxKubeVirtMemoryBytes {
		return fmt.Errorf("%w: KubeVirt max memory must be 1Gi..256Gi", ErrInvalid)
	}
	return nil
}

// ActiveKubeVirtGrant reports whether the repository holds an active grant
// and, if so, that grant's VMI budget.
func (s *Store) ActiveKubeVirtGrant(ctx context.Context, repoID int64) (KubeVirtBudget, bool, error) {
	if repoID <= 0 {
		return KubeVirtBudget{}, false, fmt.Errorf("%w: repository ID is required", ErrInvalid)
	}
	var budget KubeVirtBudget
	err := s.reader().QueryRowContext(ctx, `SELECT max_vms, max_cores, max_memory_bytes FROM kubevirt_grants WHERE repo_id=? AND revoked_at IS NULL`, repoID).
		Scan(&budget.MaxVMs, &budget.MaxCores, &budget.MaxMemoryBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return KubeVirtBudget{}, false, nil
	}
	if err != nil {
		return KubeVirtBudget{}, false, err
	}
	return budget, true, nil
}

func (s *Store) KubeVirtGrant(ctx context.Context, repoID int64) (KubeVirtGrant, error) {
	grant, err := scanKubeVirtGrant(s.reader().QueryRowContext(ctx, `SELECT repo_id, max_vms, max_cores, max_memory_bytes, granted_by, granted_at, revoked_by, revoked_at FROM kubevirt_grants WHERE repo_id=?`, repoID))
	if errors.Is(err, sql.ErrNoRows) {
		return KubeVirtGrant{}, fmt.Errorf("%w: KubeVirt grant", ErrNotFound)
	}
	return grant, err
}

func scanKubeVirtGrant(row *sql.Row) (KubeVirtGrant, error) {
	var grant KubeVirtGrant
	var grantedAt int64
	var revokedAt sql.NullInt64
	if err := row.Scan(&grant.RepoID, &grant.MaxVMs, &grant.MaxCores, &grant.MaxMemoryBytes, &grant.GrantedBy, &grantedAt, &grant.RevokedBy, &revokedAt); err != nil {
		return KubeVirtGrant{}, err
	}
	grant.GrantedAt = fromUnixNano(grantedAt)
	if revokedAt.Valid {
		at := fromUnixNano(revokedAt.Int64)
		grant.RevokedAt = &at
	}
	return grant, nil
}

// SetKubeVirtGrant changes the capability and its audit entry in one
// transaction. A grant takes budget with unset fields defaulted; granting an
// active grant again with a different budget replaces the budget and audits
// the previous one. Revocation ignores budget.
func (s *Store) SetKubeVirtGrant(ctx context.Context, repoID int64, actor string, allow bool, budget KubeVirtBudget) (KubeVirtGrant, error) {
	if repoID <= 0 || strings.TrimSpace(actor) == "" {
		return KubeVirtGrant{}, fmt.Errorf("%w: repository and actor are required", ErrInvalid)
	}
	if allow {
		budget = budget.WithDefaults()
		if err := budget.Validate(); err != nil {
			return KubeVirtGrant{}, err
		}
	} else if budget != (KubeVirtBudget{}) {
		return KubeVirtGrant{}, fmt.Errorf("%w: a KubeVirt revocation takes no budget", ErrInvalid)
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
	var previous KubeVirtBudget
	err = tx.QueryRowContext(ctx, `SELECT revoked_at, max_vms, max_cores, max_memory_bytes FROM kubevirt_grants WHERE repo_id=?`, repoID).
		Scan(&current, &previous.MaxVMs, &previous.MaxCores, &previous.MaxMemoryBytes)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return KubeVirtGrant{}, err
	}
	active := err == nil && !current.Valid
	if allow && active && previous == budget {
		return KubeVirtGrant{}, fmt.Errorf("%w: KubeVirt grant is already active with this budget", ErrInvalidState)
	}
	if !allow && !active {
		return KubeVirtGrant{}, fmt.Errorf("%w: KubeVirt grant is not active", ErrInvalidState)
	}
	if allow {
		_, err = tx.ExecContext(ctx, `INSERT INTO kubevirt_grants(repo_id,max_vms,max_cores,max_memory_bytes,granted_by,granted_at) VALUES(?,?,?,?,?,?) ON CONFLICT(repo_id) DO UPDATE SET max_vms=excluded.max_vms,max_cores=excluded.max_cores,max_memory_bytes=excluded.max_memory_bytes,granted_by=excluded.granted_by,granted_at=excluded.granted_at,revoked_by='',revoked_at=NULL`,
			repoID, budget.MaxVMs, budget.MaxCores, budget.MaxMemoryBytes, actor, now)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE kubevirt_grants SET revoked_by=?,revoked_at=? WHERE repo_id=? AND revoked_at IS NULL`, actor, now, repoID)
	}
	if err != nil {
		return KubeVirtGrant{}, err
	}
	auditDetails := map[string]any{"repo_id": repoID, "allow": allow}
	if allow {
		auditDetails["budget"] = budget
		if active {
			auditDetails["previous_budget"] = previous
		}
	}
	details, err := json.Marshal(auditDetails)
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

// ParseKubeVirtMemory reads a Kubernetes quantity such as 24Gi as a whole
// number of bytes for a grant's max memory.
func ParseKubeVirtMemory(value string) (int64, error) {
	quantity, err := resource.ParseQuantity(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%w: KubeVirt max memory %q: %w", ErrInvalid, value, err)
	}
	bytes, exact := quantity.AsInt64()
	if !exact || bytes <= 0 {
		return 0, fmt.Errorf("%w: KubeVirt max memory %q must be a positive whole number of bytes", ErrInvalid, value)
	}
	return bytes, nil
}

// MaxMemory renders the memory budget as a Kubernetes quantity (12Gi).
func (budget KubeVirtBudget) MaxMemory() string {
	return resource.NewQuantity(budget.MaxMemoryBytes, resource.BinarySI).String()
}
