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
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type WorkflowCapabilityGrant struct {
	RepoID     int64                   `json:"repo_id"`
	Capability argoworkflow.Capability `json:"capability"`
	GrantedBy  string                  `json:"granted_by"`
	GrantedAt  time.Time               `json:"granted_at"`
	RevokedBy  string                  `json:"revoked_by,omitempty"`
	RevokedAt  *time.Time              `json:"revoked_at,omitempty"`
}

const workflowCapabilitySelect = `SELECT repo_id,capability_json,granted_by,granted_at,revoked_by,revoked_at FROM workflow_capability_grants WHERE repo_id=?`

func scanWorkflowCapabilityGrant(row *sql.Row) (WorkflowCapabilityGrant, error) {
	var grant WorkflowCapabilityGrant
	var encoded string
	var granted int64
	var revoked sql.NullInt64
	if err := row.Scan(&grant.RepoID, &encoded, &grant.GrantedBy, &granted, &grant.RevokedBy, &revoked); err != nil {
		return grant, err
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&grant.Capability); err != nil {
		return grant, fmt.Errorf("invalid stored workflow capability: %w", err)
	}
	if err := grant.Capability.Validate(); err != nil {
		return grant, fmt.Errorf("invalid stored workflow capability: %w", err)
	}
	grant.GrantedAt = fromUnixNano(granted)
	if revoked.Valid {
		at := fromUnixNano(revoked.Int64)
		grant.RevokedAt = &at
	}
	return grant, nil
}

func (s *Store) WorkflowCapabilityGrant(ctx context.Context, repoID int64) (WorkflowCapabilityGrant, error) {
	grant, err := scanWorkflowCapabilityGrant(s.reader().QueryRowContext(ctx, workflowCapabilitySelect, repoID))
	if errors.Is(err, sql.ErrNoRows) {
		err = fmt.Errorf("%w: workflow capability grant", ErrNotFound)
	}
	return grant, err
}

// ActiveWorkflowCapability returns a fresh copy. Absence and revocation never
// imply a default account, resource kind or extended resource allowance.
func (s *Store) ActiveWorkflowCapability(ctx context.Context, repoID int64) (*argoworkflow.Capability, error) {
	grant, err := s.WorkflowCapabilityGrant(ctx, repoID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if grant.RevokedAt != nil {
		return nil, nil
	}
	return &grant.Capability, nil
}

// SetWorkflowCapabilityGrant atomically replaces/revokes the operator grant and
// appends its audit record. A nil capability revokes, preserving its last value.
func (s *Store) SetWorkflowCapabilityGrant(ctx context.Context, repoID int64, actor string, capability *argoworkflow.Capability) (WorkflowCapabilityGrant, error) {
	if repoID <= 0 || strings.TrimSpace(actor) == "" {
		return WorkflowCapabilityGrant{}, fmt.Errorf("%w: repository and actor are required", ErrInvalid)
	}
	if err := capability.Validate(); err != nil {
		return WorkflowCapabilityGrant{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	encoded, err := json.Marshal(capability)
	if err != nil {
		return WorkflowCapabilityGrant{}, err
	}
	if len(encoded) > 16384 {
		return WorkflowCapabilityGrant{}, fmt.Errorf("%w: capability exceeds storage bound", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowCapabilityGrant{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM repositories WHERE id=?)`, repoID).Scan(&exists); err != nil {
		return WorkflowCapabilityGrant{}, err
	}
	if !exists {
		return WorkflowCapabilityGrant{}, fmt.Errorf("%w: repository", ErrNotFound)
	}
	previous, err := scanWorkflowCapabilityGrant(tx.QueryRowContext(ctx, workflowCapabilitySelect, repoID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return WorkflowCapabilityGrant{}, err
	}
	active := err == nil && previous.RevokedAt == nil
	if capability == nil && !active {
		return WorkflowCapabilityGrant{}, fmt.Errorf("%w: workflow capability is not active", ErrInvalidState)
	}
	if capability != nil && active {
		old, marshalErr := json.Marshal(previous.Capability)
		if marshalErr != nil {
			return WorkflowCapabilityGrant{}, marshalErr
		}
		if string(old) == string(encoded) {
			return WorkflowCapabilityGrant{}, fmt.Errorf("%w: workflow capability is already active", ErrInvalidState)
		}
	}
	now := unixNano(s.now())
	action := "workflow_capability.grant"
	if capability != nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO workflow_capability_grants(repo_id,capability_json,granted_by,granted_at) VALUES(?,?,?,?) ON CONFLICT(repo_id) DO UPDATE SET capability_json=excluded.capability_json,granted_by=excluded.granted_by,granted_at=excluded.granted_at,revoked_by='',revoked_at=NULL`, repoID, string(encoded), actor, now)
	} else {
		action = "workflow_capability.revoke"
		_, err = tx.ExecContext(ctx, `UPDATE workflow_capability_grants SET revoked_by=?,revoked_at=? WHERE repo_id=? AND revoked_at IS NULL`, actor, now, repoID)
	}
	if err != nil {
		return WorkflowCapabilityGrant{}, err
	}
	details := map[string]any{"repo_id": repoID, "capability": capability}
	if active {
		details["previous_capability"] = previous.Capability
	}
	body, err := json.Marshal(details)
	if err != nil {
		return WorkflowCapabilityGrant{}, err
	}
	if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: action, ResourceType: "repository", ResourceID: fmt.Sprint(repoID), Details: string(body)}, now); err != nil {
		return WorkflowCapabilityGrant{}, err
	}
	result, err := scanWorkflowCapabilityGrant(tx.QueryRowContext(ctx, workflowCapabilitySelect, repoID))
	if err != nil {
		return WorkflowCapabilityGrant{}, err
	}
	if err = tx.Commit(); err != nil {
		return WorkflowCapabilityGrant{}, err
	}
	return result, nil
}
