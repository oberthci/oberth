package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/protectedtests"
)

type protectedRuntimeReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ReserveGenericPreflight atomically binds runtime authority to the existing
// protected lifecycle. No partially reserved legacy row can be upgraded later.
func (s *Store) ReserveGenericPreflight(ctx context.Context, admission protectedtests.GenericAdmission) (protectedtests.Execution, error) {
	if admission.Validate() != nil {
		return protectedtests.Execution{}, ErrInvalid
	}
	canonical, err := protectedtests.NewGenericAdmission(admission.Intent, &admission.Capability, admission.SupplementalGroup)
	if err != nil {
		return protectedtests.Execution{}, ErrInvalid
	}
	return s.reserveProtectedPreflight(ctx, canonical.Intent, &canonical)
}

// GenericAdmission returns nil only for a legacy execution. It is immutable
// reconstruction evidence, not a substitute for checking the current grant.
func (s *Store) GenericAdmission(ctx context.Context, runID string) (*protectedtests.GenericAdmission, error) {
	return readGenericAdmission(ctx, s.reader(), runID)
}

func readGenericAdmission(ctx context.Context, q protectedRuntimeReader, runID string) (*protectedtests.GenericAdmission, error) {
	var body, admissionDigest, intentDigest, storedIntentDigest string
	var createdAt int64
	err := q.QueryRowContext(ctx, `SELECT g.admission_json,g.admission_sha256,g.intent_sha256,v.spec_identity,g.created_at FROM protected_runtime_admissions g JOIN vm_executions v ON v.run_id=g.run_id WHERE g.run_id=?`, runID).Scan(&body, &admissionDigest, &intentDigest, &storedIntentDigest, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		var admitted bool
		if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audit_actions WHERE action='protected.runtime-admitted' AND resource_type='run' AND resource_id=?)`, runID).Scan(&admitted); err != nil {
			return nil, err
		}
		if admitted {
			return nil, fmt.Errorf("%w: audited generic runtime metadata is missing", ErrInvalidState)
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a protectedtests.GenericAdmission
	if json.Unmarshal([]byte(body), &a) != nil || a.Validate() != nil || a.Intent.RunID != runID || a.Digest() != admissionDigest || protectedtests.IntentDigest(a.Intent) != intentDigest || intentDigest != storedIntentDigest {
		return nil, fmt.Errorf("%w: corrupt protected runtime admission", ErrInvalidState)
	}
	canonical, _ := json.Marshal(a)
	if string(canonical) != body {
		return nil, fmt.Errorf("%w: noncanonical protected runtime admission", ErrInvalidState)
	}
	expected, err := genericAdmissionAuditDetails(a)
	if err != nil {
		return nil, err
	}
	var audited string
	var auditedAt int64
	if err = q.QueryRowContext(ctx, `SELECT details,created_at FROM audit_actions WHERE action='protected.runtime-admitted' AND resource_type='run' AND resource_id=?`, runID).Scan(&audited, &auditedAt); err != nil || audited != expected || createdAt != auditedAt {
		return nil, fmt.Errorf("%w: protected runtime differs from chained admission", ErrInvalidState)
	}
	return &a, nil
}

func checkGenericCapability(ctx context.Context, q protectedRuntimeReader, a protectedtests.GenericAdmission) error {
	grant, err := scanWorkflowCapabilityGrant(q.QueryRowContext(ctx, workflowCapabilitySelect, a.Intent.RepoID))
	if err != nil {
		return fmt.Errorf("%w: protected runtime capability unavailable: %w", ErrInvalidState, err)
	}
	if grant.RevokedAt != nil {
		return fmt.Errorf("%w: protected runtime capability revoked", ErrInvalidState)
	}
	if err = a.MatchCapability(&grant.Capability); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidState, err)
	}
	return nil
}
func checkStoredGenericCapability(ctx context.Context, q protectedRuntimeReader, runID string) error {
	admission, err := readGenericAdmission(ctx, q, runID)
	if err != nil || admission == nil {
		return err
	}
	return checkGenericCapability(ctx, q, *admission)
}

// CheckGenericPreflightAuthority is a fresh publication/delivery fence. Cleanup
// deliberately uses the immutable admission even after this check starts failing.
func (s *Store) CheckGenericPreflightAuthority(ctx context.Context, runID string) error {
	return checkStoredGenericCapability(ctx, s.reader(), runID)
}

func genericAdmissionAuditDetails(a protectedtests.GenericAdmission) (string, error) {
	body, err := json.Marshal(struct {
		Admission    protectedtests.GenericAdmission `json:"admission"`
		Digest       string                          `json:"admission_sha256"`
		IntentDigest string                          `json:"intent_sha256"`
	}{a, a.Digest(), protectedtests.IntentDigest(a.Intent)})
	return string(body), err
}

// Verify the state projection within the same read snapshot as the chained
// audit history. The partial audit index bounds lookups to protected admissions;
// no ordinary test workload or audit inventory is shortened.
func verifyProtectedRuntimeAdmissions(ctx context.Context, q publicationAuditQuerier) error {
	var present, admitted bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='protected_runtime_admissions')`).Scan(&present); err != nil {
		return err
	}
	if !present {
		// Historical pre-migration databases have neither table nor runtime events.
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audit_actions WHERE action='protected.runtime-admitted' AND resource_type='run')`).Scan(&admitted); err != nil {
			return err
		}
		if admitted {
			return fmt.Errorf("%w: audited protected runtime table is missing", ErrInvalidState)
		}
		return nil
	}
	var mismatch bool
	err := q.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM protected_runtime_admissions g
 LEFT JOIN audit_actions a ON a.action='protected.runtime-admitted' AND a.resource_type='run' AND a.resource_id=g.run_id
 LEFT JOIN vm_executions v ON v.run_id=g.run_id
 LEFT JOIN runs r ON r.id=g.run_id
 WHERE a.id IS NULL OR g.created_at IS NOT a.created_at OR v.execution_kind IS NOT 'kvm-preflight' OR a.actor IS NOT r.actor
 OR json_extract(a.details,'$.admission') IS NOT g.admission_json
 OR json_extract(a.details,'$.admission_sha256') IS NOT g.admission_sha256
 OR json_extract(a.details,'$.intent_sha256') IS NOT g.intent_sha256
 OR g.intent_sha256 IS NOT v.spec_identity)
 OR EXISTS(SELECT 1 FROM audit_actions a LEFT JOIN protected_runtime_admissions g ON g.run_id=a.resource_id
 WHERE a.action='protected.runtime-admitted' AND a.resource_type='run' AND g.run_id IS NULL)`).Scan(&mismatch)
	if err != nil {
		return err
	}
	if mismatch {
		return fmt.Errorf("%w: protected runtime admission differs from chained audit state", ErrInvalidState)
	}
	return nil
}
