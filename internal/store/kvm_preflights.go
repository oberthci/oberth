package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

const kvmExecutionColumns = `execution_kind,intent_json,spec_identity,workflow_uid,submitted,rejected,cleaned,receipt_json`

// ReserveKVMPreflight admits a credential-free preflight before the parent Job.
// AI-INVARIANT: the empty parent job_name is the delivery interlock even when
// release admission has already marked the parent credentialed.
func (s *Store) ReserveKVMPreflight(ctx context.Context, intent vmrunner.KVMIntent) (vmrunner.KVMExecution, error) {
	if intent.Validate() != nil || len(validation.IsDNS1123Label(intent.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(intent.WorkflowName)) != 0 {
		return vmrunner.KVMExecution{}, ErrInvalid
	}
	body, err := json.Marshal(intent)
	if err != nil || len(body) > 16384 {
		return vmrunner.KVMExecution{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return vmrunner.KVMExecution{}, err
	}
	defer func() { _ = tx.Rollback() }()
	actor, err := eligibleKVMParent(ctx, tx, intent)
	if err != nil {
		return vmrunner.KVMExecution{}, err
	}
	existing, err := scanKVMExecution(tx.QueryRowContext(ctx, `SELECT `+kvmExecutionColumns+` FROM vm_executions WHERE run_id=?`, intent.RunID))
	if err == nil {
		if vmrunner.KVMIntentDigest(existing.Intent) != vmrunner.KVMIntentDigest(intent) {
			return vmrunner.KVMExecution{}, fmt.Errorf("%w: KVM intent is immutable", ErrInvalidState)
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return vmrunner.KVMExecution{}, err
	}
	if intent.AdmittedAt.After(s.now()) || !s.now().Before(intent.Deadline) {
		return vmrunner.KVMExecution{}, ErrInvalidState
	}
	if err := claimVMCapacity(ctx, tx, intent.RunID); err != nil {
		return vmrunner.KVMExecution{}, err
	}
	now := unixNano(s.now())
	if _, err := tx.ExecContext(ctx, `INSERT INTO vm_executions(run_id,execution_kind,intent_json,spec_identity,created_at,updated_at) VALUES(?,'kvm-preflight',?,?,?,?)`, intent.RunID, string(body), vmrunner.KVMIntentDigest(intent), now, now); err != nil {
		return vmrunner.KVMExecution{}, err
	}
	value := vmrunner.KVMExecution{Intent: intent}
	if err := s.auditKVMExecution(ctx, tx, actor, "kvm.intent", value, now); err != nil {
		return vmrunner.KVMExecution{}, err
	}
	if err := tx.Commit(); err != nil {
		return vmrunner.KVMExecution{}, err
	}
	return s.KVMPreflight(ctx, intent.RunID)
}

func eligibleKVMParent(ctx context.Context, tx *sql.Tx, intent vmrunner.KVMIntent) (string, error) {
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, intent.RunID))
	if err != nil {
		return "", err
	}
	var repo string
	var upstream model.Upstream
	if err := tx.QueryRowContext(ctx, `SELECT r.name,u.name,u.kind,u.base_url FROM repositories r JOIN upstreams u ON u.id=r.upstream_id WHERE r.id=?`, run.RepoID).Scan(&repo, &upstream.Name, &upstream.Kind, &upstream.BaseURL); err != nil {
		return "", err
	}
	if run.Status != model.RunRunning || run.JobName != "" || run.RepoID != intent.RepoID || run.TestedSHA != intent.CandidateSHA || intent.Repository != upstream.QualifiedRepo(repo) {
		return "", fmt.Errorf("%w: KVM preflight requires its exact active parent before Job delivery", ErrInvalidState)
	}
	return run.Actor, nil
}

func (s *Store) KVMPreflight(ctx context.Context, runID string) (vmrunner.KVMExecution, error) {
	value, err := scanKVMExecution(s.db.QueryRowContext(ctx, `SELECT `+kvmExecutionColumns+` FROM vm_executions WHERE execution_kind='kvm-preflight' AND run_id=?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return value, err
}

// PendingKVMPreflights also returns cleaned preflights whose active parent has
// no Job, closing the crash window between cleanup and delivery. Recovery
// interrupts those parents; it never resumes publication from their receipt.
func (s *Store) PendingKVMPreflights(ctx context.Context) ([]vmrunner.KVMExecution, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+kvmExecutionColumns+` FROM vm_executions WHERE execution_kind='kvm-preflight' AND (cleaned=0 OR EXISTS (SELECT 1 FROM runs WHERE runs.id=vm_executions.run_id AND runs.status='running' AND runs.job_name='')) ORDER BY created_at,run_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var values []vmrunner.KVMExecution
	for rows.Next() {
		value, err := scanKVMExecution(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Store) SubmitKVMPreflight(ctx context.Context, runID string) error {
	return s.changeKVMExecution(ctx, runID, "kvm.create-submitted", func(tx *sql.Tx, current vmrunner.KVMExecution, now int64) error {
		if current.Submitted || current.Rejected || current.Cleaned || !fromUnixNano(now).Before(current.Intent.Deadline) {
			return ErrInvalidState
		}
		if _, err := eligibleKVMParent(ctx, tx, current.Intent); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE vm_executions SET submitted=1,updated_at=? WHERE run_id=?`, now, runID)
		return err
	})
}

func (s *Store) BindKVMPreflight(ctx context.Context, runID, uid string) error {
	if strings.TrimSpace(uid) == "" || len(uid) > 128 {
		return ErrInvalid
	}
	return s.changeKVMExecution(ctx, runID, "kvm.workflow-bound", func(tx *sql.Tx, current vmrunner.KVMExecution, now int64) error {
		if !current.Submitted || current.Rejected || current.Cleaned {
			return ErrInvalidState
		}
		if current.WorkflowUID != "" {
			if current.WorkflowUID == uid {
				return nil
			}
			return ErrInvalidState
		}
		_, err := tx.ExecContext(ctx, `UPDATE vm_executions SET workflow_uid=?,updated_at=? WHERE run_id=?`, uid, now, runID)
		return err
	})
}

// RejectKVMPreflight records only a definitive create refusal. A timeout or an
// uncertain create followed by NotFound never authorizes this transition.
func (s *Store) RejectKVMPreflight(ctx context.Context, runID string) error {
	return s.changeKVMExecution(ctx, runID, "kvm.create-refused", func(tx *sql.Tx, current vmrunner.KVMExecution, now int64) error {
		if !current.Submitted || current.Cleaned || current.WorkflowUID != "" {
			return ErrInvalidState
		}
		if current.Rejected {
			return nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE vm_executions SET rejected=1,updated_at=? WHERE run_id=?`, now, runID)
		return err
	})
}

func (s *Store) RecordKVMReceipt(ctx context.Context, runID string, receipt vmrunner.KVMReceipt) error {
	return s.changeKVMExecution(ctx, runID, "kvm.receipt", func(tx *sql.Tx, current vmrunner.KVMExecution, now int64) error {
		if !current.Submitted || current.Rejected || current.Cleaned || current.WorkflowUID == "" || receipt.WorkflowUID != current.WorkflowUID || receipt.Validate(current.Intent) != nil || receipt.FinishedAt.After(fromUnixNano(now)) {
			return ErrInvalidState
		}
		if _, err := eligibleKVMParent(ctx, tx, current.Intent); err != nil {
			return err
		}
		body, err := json.Marshal(receipt)
		if err != nil || len(body) > 16384 {
			return ErrInvalid
		}
		if current.Receipt != nil {
			previous, _ := json.Marshal(current.Receipt)
			if string(previous) == string(body) {
				return nil
			}
			return fmt.Errorf("%w: KVM receipt is immutable", ErrInvalidState)
		}
		_, err = tx.ExecContext(ctx, `UPDATE vm_executions SET receipt_json=?,updated_at=? WHERE run_id=?`, string(body), now, runID)
		return err
	})
}

// CompleteKVMPreflightCleanup is called only after the server has observed the
// exact bound Workflow and all its owned Pods absent. Unbound uncertain creates
// retain capacity; never-submitted or definitively refused intents need no UID.
func (s *Store) CompleteKVMPreflightCleanup(ctx context.Context, runID, uid string) error {
	return s.changeKVMExecution(ctx, runID, "kvm.cleanup-observed", func(tx *sql.Tx, current vmrunner.KVMExecution, now int64) error {
		if uid != current.WorkflowUID || (current.Submitted && !current.Rejected && current.WorkflowUID == "") {
			return ErrInvalidState
		}
		if current.Cleaned {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE vm_executions SET cleaning=1,cleaned=1,updated_at=? WHERE run_id=?`, now, runID); err != nil {
			return err
		}
		return releaseVMCapacity(ctx, tx, runID)
	})
}

func (s *Store) changeKVMExecution(ctx context.Context, runID, action string, change func(*sql.Tx, vmrunner.KVMExecution, int64) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanKVMExecution(tx.QueryRowContext(ctx, `SELECT `+kvmExecutionColumns+` FROM vm_executions WHERE execution_kind='kvm-preflight' AND run_id=?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var actor string
	if err := tx.QueryRowContext(ctx, `SELECT actor FROM runs WHERE id=?`, runID).Scan(&actor); err != nil {
		return err
	}
	now := unixNano(s.now())
	if err := change(tx, current, now); err != nil {
		return err
	}
	after, err := scanKVMExecution(tx.QueryRowContext(ctx, `SELECT `+kvmExecutionColumns+` FROM vm_executions WHERE execution_kind='kvm-preflight' AND run_id=?`, runID))
	if err != nil {
		return err
	}
	beforeJSON, _ := json.Marshal(current)
	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) == string(afterJSON) {
		return nil
	}
	if err := s.auditKVMExecution(ctx, tx, actor, action, after, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) auditKVMExecution(ctx context.Context, tx *sql.Tx, actor, action string, current vmrunner.KVMExecution, now int64) error {
	body, err := json.Marshal(current)
	if err != nil {
		return err
	}
	_, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: action, ResourceType: "run", ResourceID: current.Intent.RunID, Details: string(body)}, now)
	return err
}

func scanKVMExecution(row rowScanner) (vmrunner.KVMExecution, error) {
	var value vmrunner.KVMExecution
	var kind, intent, identity, receipt string
	if err := row.Scan(&kind, &intent, &identity, &value.WorkflowUID, &value.Submitted, &value.Rejected, &value.Cleaned, &receipt); err != nil {
		return value, err
	}
	if kind != "kvm-preflight" {
		return value, fmt.Errorf("%w: unexpected KVM execution kind", ErrInvalidState)
	}
	if err := json.Unmarshal([]byte(intent), &value.Intent); err != nil {
		return value, err
	}
	canonical, _ := json.Marshal(value.Intent)
	if string(canonical) != intent || value.Intent.Validate() != nil || vmrunner.KVMIntentDigest(value.Intent) != identity {
		return value, fmt.Errorf("%w: corrupted KVM intent", ErrInvalidState)
	}
	if receipt != "" {
		value.Receipt = &vmrunner.KVMReceipt{}
		if err := json.Unmarshal([]byte(receipt), value.Receipt); err != nil {
			return value, err
		}
		canonical, _ := json.Marshal(value.Receipt)
		if string(canonical) != receipt || value.Receipt.Validate(value.Intent) != nil || value.Receipt.WorkflowUID != value.WorkflowUID {
			return value, fmt.Errorf("%w: corrupted KVM receipt", ErrInvalidState)
		}
	}
	return value, nil
}

var _ vmrunner.KVMJournal = (*Store)(nil)
