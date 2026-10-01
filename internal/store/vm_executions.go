package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

var ErrVMCapacity = errors.New("store: VM capacity remains reserved")

const vmExecutionColumns = `intent_json, spec_identity, instance_json, submitted, rejected, cleaning, cleaned, created_at, updated_at`

func (s *Store) ReserveVMExecution(ctx context.Context, intent vmrunner.ExecutionIntent) (vmrunner.Execution, error) {
	if err := vmrunner.ValidateVMRunSpec(intent.Spec); err != nil {
		return vmrunner.Execution{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(validation.IsDNS1123Label(intent.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(intent.Name)) != 0 || intent.Name != vmrunner.InstanceName(intent.Spec) ||
		len(intent.ProfileIdentity) != 64 {
		return vmrunner.Execution{}, fmt.Errorf("%w: incomplete VM intent", ErrInvalid)
	}
	profile, err := hex.DecodeString(intent.ProfileIdentity)
	if err != nil || hex.EncodeToString(profile) != intent.ProfileIdentity {
		return vmrunner.Execution{}, ErrInvalid
	}
	body, err := json.Marshal(intent)
	if err != nil || len(body) > 16384 {
		return vmrunner.Execution{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return vmrunner.Execution{}, err
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := scanVMExecution(tx.QueryRowContext(ctx, `SELECT `+vmExecutionColumns+` FROM vm_executions WHERE run_id=?`, intent.Spec.RunID))
	if err == nil {
		existingBody, _ := json.Marshal(existing.Intent)
		if string(existingBody) != string(body) {
			return vmrunner.Execution{}, fmt.Errorf("%w: VM intent is immutable", ErrInvalidState)
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return vmrunner.Execution{}, err
	}
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, intent.Spec.RunID))
	if err != nil {
		return vmrunner.Execution{}, err
	}
	var repo string
	var upstream model.Upstream
	if err := tx.QueryRowContext(ctx, `SELECT r.name,u.name,u.kind,u.base_url FROM repositories r JOIN upstreams u ON u.id=r.upstream_id WHERE r.id=?`, run.RepoID).Scan(&repo, &upstream.Name, &upstream.Kind, &upstream.BaseURL); err != nil {
		return vmrunner.Execution{}, err
	}
	if run.Status != model.RunRunning || run.Credentialed || run.Release || run.TestedSHA != intent.Spec.CandidateSHA || intent.Spec.Repo != upstream.QualifiedRepo(repo) {
		return vmrunner.Execution{}, fmt.Errorf("%w: VM intent requires its exact active uncredentialed run", ErrInvalidState)
	}
	if err := claimVMCapacity(ctx, tx, intent.Spec.RunID); err != nil {
		return vmrunner.Execution{}, err
	}
	now := unixNano(s.now())
	if _, err := tx.ExecContext(ctx, `INSERT INTO vm_executions(run_id,intent_json,spec_identity,created_at,updated_at) VALUES(?,?,?,?,?)`, intent.Spec.RunID, string(body), vmrunner.SpecIdentity(intent.Spec), now, now); err != nil {
		return vmrunner.Execution{}, err
	}
	if err := s.auditVMExecution(ctx, tx, run.Actor, "vm.intent", intent.Spec.RunID, now); err != nil {
		return vmrunner.Execution{}, err
	}
	if err := tx.Commit(); err != nil {
		return vmrunner.Execution{}, err
	}
	return s.VMExecution(ctx, intent.Spec.RunID)
}

func (s *Store) VMExecution(ctx context.Context, runID string) (vmrunner.Execution, error) {
	value, err := scanVMExecution(s.db.QueryRowContext(ctx, `SELECT `+vmExecutionColumns+` FROM vm_executions WHERE run_id=?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	return value, err
}

func (s *Store) SubmitVMExecution(ctx context.Context, runID string) error {
	return s.changeVMExecution(ctx, runID, "vm.create-submitted", func(tx *sql.Tx, current vmrunner.Execution, now int64) error {
		if current.Submitted || current.Cleaning || current.Cleaned {
			return ErrInvalidState
		}
		// Credential discovery can change a running run after reservation.
		// Recheck it in the transaction that authorizes the external create.
		var eligible int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE id=? AND status='running' AND credentialed=0`, runID).Scan(&eligible); err != nil {
			return err
		}
		if eligible != 1 {
			return ErrInvalidState
		}
		_, err := tx.ExecContext(ctx, `UPDATE vm_executions SET submitted=1,updated_at=? WHERE run_id=?`, now, runID)
		return err
	})
}

// RejectVMExecution is only for a definitive transport refusal, never an
// unknown create followed by absence. The transport owns that classification.
func (s *Store) RejectVMExecution(ctx context.Context, runID string) error {
	return s.changeVMExecution(ctx, runID, "vm.create-refused", func(tx *sql.Tx, current vmrunner.Execution, now int64) error {
		if !current.Submitted || current.Cleaned || current.Instance.UID != "" {
			return ErrInvalidState
		}
		if current.Rejected {
			return nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE vm_executions SET rejected=1,updated_at=? WHERE run_id=?`, now, runID)
		return err
	})
}

func (s *Store) BindVMExecution(ctx context.Context, runID string, instance vmrunner.VMInstance) error {
	if instance.Name == "" || instance.UID == "" || len(instance.UID) > 128 || instance.CreatedAt.IsZero() {
		return ErrInvalid
	}
	return s.changeVMExecution(ctx, runID, "vm.instance-bound", func(tx *sql.Tx, current vmrunner.Execution, now int64) error {
		if !current.Submitted || current.Rejected || current.Cleaned || instance.Name != current.Intent.Name || instance.SpecIdentity != vmrunner.SpecIdentity(current.Intent.Spec) || instance.CreatedAt.After(fromUnixNano(now)) {
			return ErrInvalidState
		}
		if current.Instance.UID != "" {
			if !sameVMInstance(current.Instance, instance) {
				return ErrInvalidState
			}
			return nil
		}
		body, err := json.Marshal(instance)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE vm_executions SET instance_json=?,updated_at=? WHERE run_id=?`, string(body), now, runID)
		return err
	})
}

func (s *Store) BeginVMCleanup(ctx context.Context, runID string) error {
	return s.changeVMExecution(ctx, runID, "vm.cleanup-required", func(tx *sql.Tx, current vmrunner.Execution, now int64) error {
		if current.Cleaning || current.Cleaned {
			return nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE vm_executions SET cleaning=1,updated_at=? WHERE run_id=?`, now, runID)
		return err
	})
}

func (s *Store) CompleteVMCleanup(ctx context.Context, runID string, instance vmrunner.VMInstance) error {
	return s.changeVMExecution(ctx, runID, "vm.cleanup-observed", func(tx *sql.Tx, current vmrunner.Execution, now int64) error {
		if !sameVMInstance(current.Instance, instance) || !current.Cleaning {
			return ErrInvalidState
		}
		if current.Cleaned {
			return nil
		}
		// Only an intent never submitted can complete without a UID. An API
		// timeout and a subsequent NotFound cannot prove that create is over.
		if current.Submitted && !current.Rejected && current.Instance.UID == "" {
			return fmt.Errorf("%w: ambiguous create remains reserved", ErrInvalidState)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE vm_executions SET cleaned=1,updated_at=? WHERE run_id=?`, now, runID); err != nil {
			return err
		}
		return releaseVMCapacity(ctx, tx, runID)
	})
}

// Both execution recipes share one durable capacity reservation. An existing
// reservation cannot authorize a second recipe, even for the same run.
func claimVMCapacity(ctx context.Context, tx *sql.Tx, runID string) error {
	var occupied int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM vm_capacity_slots`).Scan(&occupied); err != nil {
		return err
	}
	if occupied != 0 {
		return ErrVMCapacity
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO vm_capacity_slots(slot,run_id) VALUES(1,?)`, runID)
	return err
}

func releaseVMCapacity(ctx context.Context, tx *sql.Tx, runID string) error {
	result, err := tx.ExecContext(ctx, `DELETE FROM vm_capacity_slots WHERE slot=1 AND run_id=?`, runID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: missing VM capacity ownership", ErrInvalidState)
	}
	return nil
}

func (s *Store) PendingVMExecutions(ctx context.Context) ([]vmrunner.Execution, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+vmExecutionColumns+` FROM vm_executions WHERE cleaned=0 ORDER BY created_at,run_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var executions []vmrunner.Execution
	for rows.Next() {
		value, err := scanVMExecution(rows)
		if err != nil {
			return nil, err
		}
		executions = append(executions, value)
	}
	return executions, rows.Err()
}

func (s *Store) changeVMExecution(ctx context.Context, runID, action string, change func(*sql.Tx, vmrunner.Execution, int64) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanVMExecution(tx.QueryRowContext(ctx, `SELECT `+vmExecutionColumns+` FROM vm_executions WHERE run_id=?`, runID))
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
	after, err := scanVMExecution(tx.QueryRowContext(ctx, `SELECT `+vmExecutionColumns+` FROM vm_executions WHERE run_id=?`, runID))
	if err != nil {
		return err
	}
	// Idempotent retries do not alter audit history.
	beforeJSON, _ := json.Marshal(current)
	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) == string(afterJSON) {
		return nil
	}
	if err := s.auditVMExecution(ctx, tx, actor, action, runID, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) auditVMExecution(ctx context.Context, tx *sql.Tx, actor, action, runID string, now int64) error {
	current, err := scanVMExecution(tx.QueryRowContext(ctx, `SELECT `+vmExecutionColumns+` FROM vm_executions WHERE run_id=?`, runID))
	if err != nil {
		return err
	}
	body, err := json.Marshal(current)
	if err != nil {
		return err
	}
	_, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: action, ResourceType: "run", ResourceID: runID, Details: string(body)}, now)
	return err
}

func scanVMExecution(row rowScanner) (vmrunner.Execution, error) {
	var value vmrunner.Execution
	var intent, identity, instance string
	var created, updated int64
	if err := row.Scan(&intent, &identity, &instance, &value.Submitted, &value.Rejected, &value.Cleaning, &value.Cleaned, &created, &updated); err != nil {
		return value, err
	}
	if err := json.Unmarshal([]byte(intent), &value.Intent); err != nil {
		return value, err
	}
	canonical, err := json.Marshal(value.Intent)
	if err != nil || string(canonical) != intent || vmrunner.ValidateVMRunSpec(value.Intent.Spec) != nil || vmrunner.SpecIdentity(value.Intent.Spec) != identity {
		return value, fmt.Errorf("%w: corrupted VM intent", ErrInvalidState)
	}
	if instance != "" {
		if err := json.Unmarshal([]byte(instance), &value.Instance); err != nil {
			return value, err
		}
		if value.Instance.Name != value.Intent.Name || value.Instance.SpecIdentity != identity || value.Instance.UID == "" {
			return value, fmt.Errorf("%w: corrupted VM instance", ErrInvalidState)
		}
	}
	value.CreatedAt = fromUnixNano(created)
	value.UpdatedAt = fromUnixNano(updated)
	return value, nil
}

var _ vmrunner.ExecutionJournal = (*Store)(nil)

func sameVMInstance(a, b vmrunner.VMInstance) bool {
	return a.Name == b.Name && a.UID == b.UID && a.SpecIdentity == b.SpecIdentity && a.CreatedAt.Equal(b.CreatedAt)
}
