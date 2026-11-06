package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/protectedtests"
)

type placedWorkflowRecord struct {
	RunID     string          `json:"run_id"`
	Placement string          `json:"placement_sha256"`
	SourceUID string          `json:"source_uid"`
	Workflow  json.RawMessage `json:"workflow"`
}

type placedWorkflowUID struct {
	Intent string `json:"intent_sha256"`
	UID    string `json:"uid"`
}

func workflowRecordDigest(body string) string {
	digest := sha256.Sum256([]byte(body))
	return hex.EncodeToString(digest[:])
}

func placementWorkflowRecord(ctx context.Context, q protectedRuntimeReader, a protectedtests.PlacementAdmission, name string, body []byte) (string, error) {
	scope, err := a.Scope()
	if err != nil {
		return "", err
	}
	if name != scope.NormalWorkflow || len(body) == 0 || len(body) > 4<<20 {
		return "", ErrInvalid
	}
	var wf struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name            string            `json:"name"`
			Namespace       string            `json:"namespace"`
			UID             string            `json:"uid"`
			ResourceVersion string            `json:"resourceVersion"`
			Annotations     map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &wf) != nil || wf.Kind != "Workflow" || wf.APIVersion != "argoproj.io/v1alpha1" || wf.Metadata.Name != name || wf.Metadata.Namespace != scope.Namespace || wf.Metadata.UID != "" || wf.Metadata.ResourceVersion != "" || wf.Metadata.Annotations["oberth.ci/run-id"] != scope.RunID || len(wf.Metadata.Annotations["oberth.ci/spec-identity"]) != 64 {
		return "", ErrInvalid
	}
	source, err := readPlacementSource(ctx, q, a, name)
	if err != nil {
		return "", err
	}
	if source == "" {
		return "", ErrInvalidState
	}
	encoded, err := json.Marshal(placedWorkflowRecord{scope.RunID, a.Digest(), source, body})
	return string(encoded), err
}

// PreparePlacementWorkflow commits the exact intended normal phase before POST.
// It cannot be replaced by a run annotation or a newly observed Workflow UID.
func (s *Store) PreparePlacementWorkflow(ctx context.Context, runID, name string, body []byte) error {
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
	expected, err := placementWorkflowRecord(ctx, tx, *a, name, body)
	if err != nil {
		return err
	}
	if err = checkStoredGenericCapability(ctx, tx, runID, s.now()); err != nil {
		return err
	}
	execution, err := scanKVMExecution(tx.QueryRowContext(ctx, `SELECT `+kvmExecutionColumns+` FROM vm_executions WHERE run_id=?`, runID))
	if err != nil {
		return err
	}
	if !execution.Cleaned || execution.Receipt == nil || execution.Refusal != nil {
		return ErrInvalidState
	}
	var prior string
	err = tx.QueryRowContext(ctx, `SELECT details FROM audit_actions WHERE action='protected.placement-workflow' AND resource_type='workflow' AND resource_id=?`, name).Scan(&prior)
	if err == nil {
		if prior != expected {
			return ErrInvalidState
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var actor string
	if err = tx.QueryRowContext(ctx, `SELECT actor FROM runs WHERE id=?`, runID).Scan(&actor); err != nil {
		return err
	}
	if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "protected.placement-workflow", ResourceType: "workflow", ResourceID: name, Details: expected}, s.now().UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}

func readPlacedWorkflow(ctx context.Context, q protectedRuntimeReader, a protectedtests.PlacementAdmission, name string) ([]byte, string, error) {
	var body string
	err := q.QueryRowContext(ctx, `SELECT details FROM audit_actions WHERE action='protected.placement-workflow' AND resource_type='workflow' AND resource_id=?`, name).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var record placedWorkflowRecord
	if json.Unmarshal([]byte(body), &record) != nil {
		return nil, "", ErrInvalidState
	}
	expected, err := placementWorkflowRecord(ctx, q, a, name, record.Workflow)
	if err != nil {
		return nil, "", err
	}
	if body != expected {
		return nil, "", ErrInvalidState
	}
	return record.Workflow, workflowRecordDigest(body), nil
}

func readPlacedWorkflowUID(ctx context.Context, q protectedRuntimeReader, name, digest string) (string, error) {
	var body string
	err := q.QueryRowContext(ctx, `SELECT details FROM audit_actions WHERE action='protected.placement-workflow-bound' AND resource_type='workflow' AND resource_id=?`, name).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var bound placedWorkflowUID
	if json.Unmarshal([]byte(body), &bound) != nil || bound.UID == "" || len(bound.UID) > 128 || bound.Intent != digest {
		return "", ErrInvalidState
	}
	canonical, _ := json.Marshal(bound)
	if string(canonical) != body {
		return "", ErrInvalidState
	}
	return bound.UID, nil
}

// PlacementWorkflow is immutable observation/disposal custody, never authority
// to create a replacement or to reuse a receipt under changed policy.
func (s *Store) PlacementWorkflow(ctx context.Context, runID, name string) ([]byte, string, error) {
	a, err := s.PlacementAdmission(ctx, runID)
	if err != nil {
		return nil, "", err
	}
	if a == nil {
		return nil, "", ErrInvalidState
	}
	body, digest, err := readPlacedWorkflow(ctx, s.reader(), *a, name)
	if err != nil {
		return nil, "", err
	}
	uid, err := readPlacedWorkflowUID(ctx, s.reader(), name, digest)
	return body, uid, err
}

// BindPlacementWorkflowUID may run after revocation solely to settle an exact
// uncertain POST. The controller must first compare the retained full spec and
// original environment; this journal cannot grant any allocation permission.
func (s *Store) BindPlacementWorkflowUID(ctx context.Context, runID, name, uid string) error {
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
	body, digest, err := readPlacedWorkflow(ctx, tx, *a, name)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return ErrInvalidState
	}
	prior, err := readPlacedWorkflowUID(ctx, tx, name, digest)
	if err != nil {
		return err
	}
	if prior != "" {
		if prior != uid {
			return ErrInvalidState
		}
		return nil
	}
	encoded, _ := json.Marshal(placedWorkflowUID{digest, uid})
	var actor string
	if err = tx.QueryRowContext(ctx, `SELECT actor FROM runs WHERE id=?`, runID).Scan(&actor); err != nil {
		return err
	}
	if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "protected.placement-workflow-bound", ResourceType: "workflow", ResourceID: name, Details: string(encoded)}, s.now().UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}
