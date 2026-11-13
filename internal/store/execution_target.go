package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// ErrExecutionTargetQuarantined identifies retained migration evidence that
// cannot authorize routing or effects. Other invalid states are not quarantine.
var ErrExecutionTargetQuarantined = fmt.Errorf("%w: execution target binding is quarantined", ErrInvalidState)

// BindExecutionTarget atomically checks the canonical run and creates one
// immutable route. Identical calls are idempotent; no table grants authority.
func (s *Store) BindExecutionTarget(ctx context.Context, binding model.ExecutionTargetBinding) error {
	body, err := json.Marshal(binding)
	if err != nil || len(body) > 2<<20 || binding.RunID == "" || binding.Workflow == "" || binding.Target == "" || binding.RepositoryID <= 0 || binding.Namespace == "" || !json.Valid(binding.Snapshot) {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var repo int64
	var ref, sha, tested, workflow, kind, trigger, schedule string
	var release bool
	if err := tx.QueryRowContext(ctx, `SELECT repo_id,ref,sha,tested_sha,job_name,ref_kind,release,trigger,schedule_entry FROM runs WHERE id=?`, binding.RunID).Scan(&repo, &ref, &sha, &tested, &workflow, &kind, &release, &trigger, &schedule); err != nil {
		return err
	}
	if tested == "" {
		tested = sha
	}
	qualified, err := qualifiedRepoName(ctx, tx, repo)
	if err != nil {
		return err
	}
	if qualified != binding.Repository {
		return ErrInvalid
	}
	if repo != binding.RepositoryID || ref != binding.Ref || sha != binding.SHA || tested != binding.TestedSHA || workflow != binding.Workflow || kind != binding.RefKind || trigger != binding.Trigger || schedule != binding.ScheduleEntry || (binding.Target != "local" && (kind != string(model.RefBranch) || trigger != "branch" || schedule != "" || release)) {
		return ErrInvalid
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO execution_target_bindings(run_id,workflow_name,binding_json) VALUES(?,?,?) ON CONFLICT(run_id) DO NOTHING`, binding.RunID, binding.Workflow, string(body))
	if err != nil {
		return err
	}
	var saved, quarantine string
	if err := tx.QueryRowContext(ctx, `SELECT binding_json,quarantine_reason FROM execution_target_bindings WHERE run_id=?`, binding.RunID).Scan(&saved, &quarantine); err != nil {
		return err
	}
	if quarantine != "" {
		return executionTargetQuarantineError()
	}
	if saved != string(body) {
		return ErrInvalid
	}
	if count, _ := result.RowsAffected(); count == 1 {
		if err := s.auditTarget(ctx, tx, binding.RunID, "bound", binding); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ExecutionTarget(ctx context.Context, runID, workflow string) (model.ExecutionTargetBinding, bool, error) {
	var body, custodyDigest, quarantine string
	query, value := `SELECT binding_json,custody_digest,quarantine_reason FROM execution_target_bindings WHERE run_id=?`, runID
	if runID == "" {
		query, value = `SELECT binding_json,custody_digest,quarantine_reason FROM execution_target_bindings WHERE workflow_name=?`, workflow
	}
	err := s.db.QueryRowContext(ctx, query, value).Scan(&body, &custodyDigest, &quarantine)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ExecutionTargetBinding{}, false, nil
	}
	var binding model.ExecutionTargetBinding
	if err != nil {
		return binding, false, err
	}
	if quarantine != "" {
		return binding, true, executionTargetQuarantineError()
	}
	if json.Unmarshal([]byte(body), &binding) != nil || (workflow != "" && binding.Workflow != workflow) {
		return binding, false, ErrInvalid
	}
	binding.CustodyDigest = custodyDigest
	return binding, true, nil
}

func (s *Store) ExecutionTargetCancelled(ctx context.Context, runID string) (bool, error) {
	var cancelled bool
	err := s.db.QueryRowContext(ctx, `SELECT cancelled FROM execution_target_bindings WHERE run_id=?`, runID).Scan(&cancelled)
	return cancelled, err
}
func (s *Store) CancelExecutionTarget(ctx context.Context, runID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var cancelled bool
	if err := tx.QueryRowContext(ctx, `SELECT cancelled FROM execution_target_bindings WHERE run_id=?`, runID).Scan(&cancelled); err != nil {
		return err
	}
	if cancelled {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE execution_target_bindings SET cancelled=1 WHERE run_id=?`, runID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if count != 1 {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := s.auditTarget(ctx, tx, runID, "cancelled", map[string]bool{"cancelled": true}); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) BindExecutionTargetAdmission(ctx context.Context, runID string, body []byte) error {
	if len(body) > 2<<20 || !json.Valid(body) {
		return ErrInvalid
	}
	result, err := s.db.ExecContext(ctx, `UPDATE execution_target_bindings SET admission_json=? WHERE run_id=? AND quarantine_reason='' AND cancelled=0 AND (admission_json='' OR admission_json=?)`, string(body), runID, string(body))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if count != 1 {
		return ErrInvalid
	}
	return err
}

// PrepareExecutionTargetResource records a single possible POST before effects.
// A repeated intent never authorizes a second POST, including after restart.
func (s *Store) PrepareExecutionTargetResource(ctx context.Context, resource model.ExecutionTargetResource) (bool, error) {
	if resource.RunID == "" || resource.Namespace == "" || resource.Name == "" || resource.NotBefore.IsZero() || len(resource.Object) > 4<<20 || !json.Valid(resource.Object) {
		return false, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var cancelled bool
	var admission, body, quarantine string
	if err := tx.QueryRowContext(ctx, `SELECT cancelled,admission_json,binding_json,quarantine_reason FROM execution_target_bindings WHERE run_id=?`, resource.RunID).Scan(&cancelled, &admission, &body, &quarantine); err != nil {
		return false, err
	}
	var binding model.ExecutionTargetBinding
	if quarantine != "" {
		return false, executionTargetQuarantineError()
	}
	if cancelled || admission == "" || json.Unmarshal([]byte(body), &binding) != nil || resource.Namespace != binding.Namespace {
		return false, ErrInvalid
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO execution_target_resources(run_id,kind,namespace,name,object_json,not_before) VALUES(?,?,?,?,?,?) ON CONFLICT(run_id,kind,namespace,name) DO NOTHING`, resource.RunID, resource.Kind, resource.Namespace, resource.Name, string(resource.Object), resource.NotBefore.UnixNano())
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	var old string
	if err := tx.QueryRowContext(ctx, `SELECT object_json FROM execution_target_resources WHERE run_id=? AND kind=? AND namespace=? AND name=?`, resource.RunID, resource.Kind, resource.Namespace, resource.Name).Scan(&old); err != nil {
		return false, err
	}
	if old != string(resource.Object) {
		return false, ErrInvalid
	}
	if count == 1 {
		if err := s.auditTarget(ctx, tx, resource.RunID, "create-intent", resource); err != nil {
			return false, err
		}
	}
	return count == 1, tx.Commit()
}
func (s *Store) ObserveExecutionTargetUID(ctx context.Context, runID, kind, namespace, name, uid string) error {
	if uid == "" {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireUsableExecutionTarget(ctx, tx, runID); err != nil {
		return err
	}
	var old string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM execution_target_resources WHERE run_id=? AND kind=? AND namespace=? AND name=?`, runID, kind, namespace, name).Scan(&old); err != nil {
		return err
	}
	if old == uid {
		return nil
	}
	if old != "" {
		return ErrInvalid
	}
	result, err := tx.ExecContext(ctx, `UPDATE execution_target_resources SET uid=? WHERE run_id=? AND kind=? AND namespace=? AND name=? AND (uid='' OR uid=?)`, uid, runID, kind, namespace, name, uid)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if count != 1 {
		return ErrInvalid
	}
	if err != nil {
		return err
	}
	if err := s.auditTarget(ctx, tx, runID, "uid-observed", map[string]string{"kind": kind, "namespace": namespace, "name": name, "uid": uid}); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ExecutionTargetResources(ctx context.Context, runID string) ([]model.ExecutionTargetResource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind,namespace,name,uid,object_json,not_before FROM execution_target_resources WHERE run_id=? ORDER BY kind,name`, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []model.ExecutionTargetResource
	for rows.Next() {
		r := model.ExecutionTargetResource{RunID: runID}
		var body string
		var stamp int64
		if err := rows.Scan(&r.Kind, &r.Namespace, &r.Name, &r.UID, &body, &stamp); err != nil {
			return nil, err
		}
		r.Object = json.RawMessage(body)
		r.NotBefore = time.Unix(0, stamp).UTC()
		result = append(result, r)
	}
	return result, rows.Err()
}

// RetainExecutionTargetOwnedResource records native descendants even after
// cancellation. It never authorizes a POST: the original Workflow UID must
// already be durable and appear in this object's actual owner references.
func (s *Store) RetainExecutionTargetOwnedResource(ctx context.Context, r model.ExecutionTargetResource, workflowUID string) error {
	if workflowUID == "" || r.UID == "" || r.Name == "" || (r.Kind != "Pod" && r.Kind != "PersistentVolumeClaim" && r.Kind != "VirtualMachineInstance") || len(r.Object) > 4<<20 {
		return ErrInvalid
	}
	var object struct {
		Metadata struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
			UID       string `json:"uid"`
			Owners    []struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
				UID        string `json:"uid"`
			} `json:"ownerReferences"`
		} `json:"metadata"`
	}
	if json.Unmarshal(r.Object, &object) != nil || object.Metadata.Namespace != r.Namespace || object.Metadata.Name != r.Name || object.Metadata.UID != r.UID {
		return ErrInvalid
	}
	owned := false
	for _, owner := range object.Metadata.Owners {
		if owner.APIVersion == "argoproj.io/v1alpha1" && owner.Kind == "Workflow" && owner.UID == workflowUID {
			owned = true
		}
	}
	if !owned {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireUsableExecutionTarget(ctx, tx, r.RunID); err != nil {
		return err
	}
	var parentUID, body string
	if err := tx.QueryRowContext(ctx, `SELECT r.uid,b.binding_json FROM execution_target_resources r JOIN execution_target_bindings b ON b.run_id=r.run_id WHERE r.run_id=? AND r.kind='Workflow'`, r.RunID).Scan(&parentUID, &body); err != nil {
		return err
	}
	var binding model.ExecutionTargetBinding
	if parentUID != workflowUID || json.Unmarshal([]byte(body), &binding) != nil || r.Namespace != binding.Namespace {
		return ErrInvalid
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM execution_target_resources WHERE run_id=? AND NOT(kind=? AND namespace=? AND name=?)`, r.RunID, r.Kind, r.Namespace, r.Name).Scan(&count); err != nil {
		return err
	}
	if count >= 1024 {
		return ErrInvalid
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO execution_target_resources(run_id,kind,namespace,name,uid,object_json,not_before) VALUES(?,?,?,?,?,?,?) ON CONFLICT(run_id,kind,namespace,name) DO NOTHING`, r.RunID, r.Kind, r.Namespace, r.Name, r.UID, string(r.Object), r.NotBefore.UnixNano()); err != nil {
		return err
	}
	var uid string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM execution_target_resources WHERE run_id=? AND kind=? AND namespace=? AND name=?`, r.RunID, r.Kind, r.Namespace, r.Name).Scan(&uid); err != nil {
		return err
	}
	if uid != "" && uid != r.UID {
		return ErrInvalid
	}
	if _, err := tx.ExecContext(ctx, `UPDATE execution_target_resources SET uid=? WHERE run_id=? AND kind=? AND namespace=? AND name=?`, r.UID, r.RunID, r.Kind, r.Namespace, r.Name); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) auditTarget(ctx context.Context, tx *sql.Tx, runID, action string, details any) error {
	body, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: "oberth", Action: "ci.execution-target." + action, ResourceType: "run", ResourceID: runID, Details: string(body)}, unixNano(s.now()))
	return err
}

// NextExecutionTargetCleanup rotates terminal work without a second allocator.
func (s *Store) NextExecutionTargetCleanup(ctx context.Context, after string) (model.ExecutionTargetBinding, bool, error) {
	var body, custodyDigest string
	err := s.db.QueryRowContext(ctx, `SELECT b.binding_json,b.custody_digest FROM execution_target_bindings b JOIN runs r ON r.id=b.run_id WHERE r.status IN ('passed','failed','interrupted') AND b.quarantine_reason='' AND b.cleanup_receipt='' AND EXISTS(SELECT 1 FROM execution_target_resources x WHERE x.run_id=b.run_id) ORDER BY (b.run_id<=?),b.run_id LIMIT 1`, after).Scan(&body, &custodyDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ExecutionTargetBinding{}, false, nil
	}
	if err != nil {
		return model.ExecutionTargetBinding{}, false, err
	}
	var b model.ExecutionTargetBinding
	if json.Unmarshal([]byte(body), &b) != nil {
		return b, false, ErrInvalid
	}
	b.CustodyDigest = custodyDigest
	return b, true, nil
}

// CompleteExecutionTargetCleanup retains the final original UID inventory in
// an immutable receipt and audit chain before releasing the retention fence.
// The caller must have freshly authenticated absence through the bound clients.
func (s *Store) CompleteExecutionTargetCleanup(ctx context.Context, runID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireUsableExecutionTarget(ctx, tx, runID); err != nil {
		return err
	}
	var cancelled bool
	var receipt string
	if err := tx.QueryRowContext(ctx, `SELECT cancelled,cleanup_receipt FROM execution_target_bindings WHERE run_id=?`, runID).Scan(&cancelled, &receipt); err != nil {
		return err
	}
	if !cancelled {
		return ErrInvalid
	}
	if receipt != "" {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT kind,namespace,name,uid,object_json,not_before FROM execution_target_resources WHERE run_id=? ORDER BY kind,name`, runID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	inventory := []model.ExecutionTargetResource{}
	for rows.Next() {
		r := model.ExecutionTargetResource{RunID: runID}
		var stamp int64
		var body string
		if err := rows.Scan(&r.Kind, &r.Namespace, &r.Name, &r.UID, &body, &stamp); err != nil {
			return err
		}
		r.Object = json.RawMessage(body)
		r.NotBefore = time.Unix(0, stamp).UTC()
		inventory = append(inventory, r)
	}
	err = rows.Err()
	if err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		ObservedAt time.Time
		Resources  []model.ExecutionTargetResource
	}{s.now().UTC(), inventory})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE execution_target_bindings SET cleanup_receipt=? WHERE run_id=?`, string(body), runID); err != nil {
		return err
	}
	if err := s.auditTarget(ctx, tx, runID, "cleanup-confirmed", json.RawMessage(body)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM execution_target_resources WHERE run_id=?`, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func executionTargetQuarantineError() error {
	return fmt.Errorf("%w; %s", ErrExecutionTargetQuarantined, model.ExecutionTargetRecoveryRemedy)
}

func requireUsableExecutionTarget(ctx context.Context, tx *sql.Tx, runID string) error {
	var reason string
	if err := tx.QueryRowContext(ctx, `SELECT quarantine_reason FROM execution_target_bindings WHERE run_id=?`, runID).Scan(&reason); err != nil {
		return err
	}
	if reason != "" {
		return executionTargetQuarantineError()
	}
	return nil
}

func (s *Store) ExecutionTargetRecoveryStatus(ctx context.Context) (*model.ExecutionTargetRecoveryStatus, error) {
	var count int64
	if err := s.reader().QueryRowContext(ctx, `SELECT count(*) FROM execution_target_bindings WHERE quarantine_reason!=''`).Scan(&count); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	return &model.ExecutionTargetRecoveryStatus{QuarantinedBindings: count, Reason: model.ExecutionTargetRecoveryReason, Remedy: model.ExecutionTargetRecoveryRemedy}, nil
}
