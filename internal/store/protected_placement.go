package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/protectedtests"
)

// ReservePlacedGenericPreflight commits placement, runtime, lifecycle, capacity
// and both audit bindings atomically. It cannot attach custody after a create.
func (s *Store) ReservePlacedGenericPreflight(ctx context.Context, runtime protectedtests.GenericAdmission, placement protectedtests.PlacementAdmission) (protectedtests.Execution, error) {
	if placement.Validate(runtime) != nil {
		return protectedtests.Execution{}, ErrInvalid
	}
	return s.reserveProtectedPreflight(ctx, runtime.Intent, &runtime, &placement)
}

func checkPlacementParent(ctx context.Context, tx *sql.Tx, a protectedtests.PlacementAdmission) error {
	scope, err := a.Scope()
	if err != nil {
		return err
	}
	var ref, kind, trigger, schedule string
	var release, credentialed bool
	if err = tx.QueryRowContext(ctx, `SELECT ref,ref_kind,trigger,schedule_entry,release,credentialed FROM runs WHERE id=?`, scope.RunID).Scan(&ref, &kind, &trigger, &schedule, &release, &credentialed); err != nil {
		return err
	}
	if ref != scope.Ref || kind != string(model.RefBranch) || trigger != "branch" || schedule != "" || release || credentialed {
		return fmt.Errorf("%w: protected placement requires exact ordinary branch parent", ErrInvalidState)
	}
	return nil
}

// PlacementAdmission reconstructs custody only; current authority is checked
// independently at every actual allocation, receipt and delivery boundary.
func (s *Store) PlacementAdmission(ctx context.Context, runID string) (*protectedtests.PlacementAdmission, error) {
	return readPlacementAdmission(ctx, s.reader(), runID)
}
func readPlacementAdmission(ctx context.Context, q protectedRuntimeReader, runID string) (*protectedtests.PlacementAdmission, error) {
	var body, digest, runtimeDigest, intentDigest string
	var createdAt int64
	err := q.QueryRowContext(ctx, `SELECT admission_json,admission_sha256,runtime_sha256,intent_sha256,created_at FROM protected_placement_admissions WHERE run_id=?`, runID).Scan(&body, &digest, &runtimeDigest, &intentDigest, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		var admitted bool
		if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audit_actions WHERE action='protected.placement-admitted' AND resource_type='run' AND resource_id=?)`, runID).Scan(&admitted); err != nil {
			return nil, err
		}
		if admitted {
			return nil, fmt.Errorf("%w: audited placement metadata is missing", ErrInvalidState)
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	runtime, err := readGenericAdmission(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	var a protectedtests.PlacementAdmission
	if runtime == nil || json.Unmarshal([]byte(body), &a) != nil || a.Validate(*runtime) != nil || a.Digest() != digest || a.RuntimeSHA256 != runtimeDigest || a.IntentSHA256 != intentDigest {
		return nil, fmt.Errorf("%w: corrupt protected placement custody", ErrInvalidState)
	}
	canonical, _ := json.Marshal(a)
	if string(canonical) != body {
		return nil, fmt.Errorf("%w: noncanonical placement custody", ErrInvalidState)
	}
	expected, err := placementAdmissionAuditDetails(a)
	if err != nil {
		return nil, err
	}
	var audited string
	var auditedAt int64
	if err = q.QueryRowContext(ctx, `SELECT details,created_at FROM audit_actions WHERE action='protected.placement-admitted' AND resource_type='run' AND resource_id=?`, runID).Scan(&audited, &auditedAt); err != nil || audited != expected || auditedAt != createdAt {
		return nil, fmt.Errorf("%w: placement custody differs from chained admission", ErrInvalidState)
	}
	return &a, nil
}
func placementAdmissionAuditDetails(a protectedtests.PlacementAdmission) (string, error) {
	body, err := json.Marshal(struct {
		Admission     protectedtests.PlacementAdmission `json:"admission"`
		Digest        string                            `json:"admission_sha256"`
		RuntimeDigest string                            `json:"runtime_sha256"`
		IntentDigest  string                            `json:"intent_sha256"`
	}{a, a.Digest(), a.RuntimeSHA256, a.IntentSHA256})
	return string(body), err
}
func (s *Store) insertPlacementAdmission(ctx context.Context, tx *sql.Tx, actor, runID string, a protectedtests.PlacementAdmission, now int64) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO protected_placement_admissions(run_id,admission_json,admission_sha256,runtime_sha256,intent_sha256,created_at) VALUES(?,?,?,?,?,?)`, runID, string(body), a.Digest(), a.RuntimeSHA256, a.IntentSHA256, now); err != nil {
		return err
	}
	details, err := placementAdmissionAuditDetails(a)
	if err != nil {
		return err
	}
	_, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "protected.placement-admitted", ResourceType: "run", ResourceID: runID, Details: details}, now)
	return err
}

func verifyProtectedPlacementAdmissions(ctx context.Context, q publicationAuditQuerier) error {
	var present, admitted bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='protected_placement_admissions')`).Scan(&present); err != nil {
		return err
	}
	if !present {
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audit_actions WHERE action='protected.placement-admitted' AND resource_type='run')`).Scan(&admitted); err != nil {
			return err
		}
		if admitted {
			return fmt.Errorf("%w: audited placement table is missing", ErrInvalidState)
		}
		return nil
	}
	var mismatch bool
	err := q.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM protected_placement_admissions p
 LEFT JOIN audit_actions a ON a.action='protected.placement-admitted' AND a.resource_type='run' AND a.resource_id=p.run_id
 LEFT JOIN protected_runtime_admissions g ON g.run_id=p.run_id
 LEFT JOIN vm_executions v ON v.run_id=p.run_id
 LEFT JOIN runs r ON r.id=p.run_id
 WHERE a.id IS NULL OR p.created_at IS NOT a.created_at OR a.actor IS NOT r.actor
 OR json_extract(a.details,'$.admission') IS NOT p.admission_json
 OR json_extract(a.details,'$.admission_sha256') IS NOT p.admission_sha256
 OR json_extract(a.details,'$.runtime_sha256') IS NOT p.runtime_sha256
 OR json_extract(a.details,'$.intent_sha256') IS NOT p.intent_sha256
 OR p.runtime_sha256 IS NOT g.admission_sha256 OR p.intent_sha256 IS NOT g.intent_sha256
 OR p.intent_sha256 IS NOT v.spec_identity OR p.created_at IS NOT g.created_at)
 OR EXISTS(SELECT 1 FROM audit_actions a LEFT JOIN protected_placement_admissions p ON p.run_id=a.resource_id
 WHERE a.action='protected.placement-admitted' AND a.resource_type='run' AND p.run_id IS NULL)`).Scan(&mismatch)
	if err != nil {
		return err
	}
	if mismatch {
		return fmt.Errorf("%w: protected placement differs from chained audit state", ErrInvalidState)
	}
	return nil
}

// PlacementAdmissionForWorkflow retains custody even when the primary policy
// is removed before a token refresh or a name-only observation.
func (s *Store) PlacementAdmissionForWorkflow(ctx context.Context, name string) (*protectedtests.PlacementAdmission, error) {
	var id string
	err := s.reader().QueryRowContext(ctx, `SELECT run_id FROM protected_placement_admissions WHERE json_extract(admission_json,'$.placement.normal_workflow')=? OR json_extract(admission_json,'$.placement.preflight_workflow')=?`, name, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.PlacementAdmission(ctx, id)
}

// BindPlacementSource records the API-assigned source volume UID before any
// seed Pod or Workflow can consume it. The chained audit record is the sole
// immutable custody record; no mutable projection can replace its UID.
func (s *Store) BindPlacementSource(ctx context.Context, runID, workflow, uid string) error {
	if uid == "" || len(uid) > 128 {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := readPlacementAdmission(ctx, tx, runID)
	if err != nil {
		return err
	}
	if a == nil {
		return ErrInvalidState
	}
	expected, err := placementSourceDetails(*a, workflow, uid)
	if err != nil {
		return err
	}
	prior, err := readPlacementSource(ctx, tx, *a, workflow)
	if err != nil {
		return err
	}
	if prior != "" {
		if prior != uid {
			return ErrInvalidState
		}
		return nil
	}
	if err = checkStoredGenericCapability(ctx, tx, runID, s.now()); err != nil {
		return err
	}
	var actor string
	if err = tx.QueryRowContext(ctx, `SELECT actor FROM runs WHERE id=?`, runID).Scan(&actor); err != nil {
		return err
	}
	_, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "protected.placement-source", ResourceType: "workflow", ResourceID: workflow, Details: expected}, s.now().UnixNano())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PlacementSourceUID(ctx context.Context, runID, workflow string) (string, error) {
	a, err := s.PlacementAdmission(ctx, runID)
	if err != nil {
		return "", err
	}
	if a == nil {
		return "", ErrInvalidState
	}
	return readPlacementSource(ctx, s.reader(), *a, workflow)
}

func placementSourceDetails(a protectedtests.PlacementAdmission, workflow, uid string) (string, error) {
	scope, err := a.Scope()
	if err != nil {
		return "", err
	}
	if workflow != scope.PreflightWorkflow && workflow != scope.NormalWorkflow {
		return "", ErrInvalidState
	}
	body, err := json.Marshal(struct {
		RunID     string `json:"run_id"`
		Workflow  string `json:"workflow"`
		UID       string `json:"uid"`
		Placement string `json:"placement_sha256"`
	}{scope.RunID, workflow, uid, a.Digest()})
	return string(body), err
}

func readPlacementSource(ctx context.Context, q protectedRuntimeReader, a protectedtests.PlacementAdmission, workflow string) (string, error) {
	var body string
	err := q.QueryRowContext(ctx, `SELECT details FROM audit_actions WHERE action='protected.placement-source' AND resource_type='workflow' AND resource_id=?`, workflow).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var value struct {
		UID string `json:"uid"`
	}
	if json.Unmarshal([]byte(body), &value) != nil || value.UID == "" || len(value.UID) > 128 {
		return "", ErrInvalidState
	}
	expected, err := placementSourceDetails(a, workflow, value.UID)
	if err != nil {
		return "", err
	}
	if body != expected {
		return "", ErrInvalidState
	}
	return value.UID, nil
}
