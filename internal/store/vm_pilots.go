package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

var _ vmrunner.PilotJournal = (*Store)(nil)

const pilotColumns = `plan_json,plan_identity,submitted,cleaning,cleaned,failure,attempt_json,receipt_json,endpoint_json,restart_json,attach_json,created_at,updated_at`
const pilotResourceColumns = `resource_key,intent_json,receipt_json,uid,submitted,rejected,cleaning,cleaned`

type pilotState struct {
	Execution vmrunner.PilotExecution
	Resources []vmrunner.PilotResource
}

func (s *Store) ReservePilot(ctx context.Context, plan vmrunner.PilotPlan) (vmrunner.PilotExecution, error) {
	if err := vmrunner.ValidatePilotPlan(plan); err != nil {
		return vmrunner.PilotExecution{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return vmrunner.PilotExecution{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanPilot(tx.QueryRowContext(ctx, `SELECT `+pilotColumns+` FROM vm_suite_executions WHERE run_id=?`, plan.Spec.RunID))
	if err == nil {
		if vmrunner.PilotIdentity(current.Plan) != vmrunner.PilotIdentity(plan) {
			return vmrunner.PilotExecution{}, fmt.Errorf("%w: sealed pilot plan is immutable", ErrInvalidState)
		}
		return current, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return vmrunner.PilotExecution{}, err
	}
	if err := pilotRunEligible(ctx, tx, plan, true); err != nil {
		return vmrunner.PilotExecution{}, err
	}
	if err := claimVMCapacity(ctx, tx, plan.Spec.RunID); err != nil {
		return vmrunner.PilotExecution{}, err
	}
	now := s.now().UTC()
	body, err := json.Marshal(plan)
	if err != nil {
		return vmrunner.PilotExecution{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO vm_suite_executions(run_id,plan_json,plan_identity,created_at,updated_at) VALUES(?,?,?,?,?)`, plan.Spec.RunID, string(body), vmrunner.PilotIdentity(plan), unixNano(now), unixNano(now)); err != nil {
		return vmrunner.PilotExecution{}, err
	}
	current = vmrunner.PilotExecution{Plan: plan, CreatedAt: now, UpdatedAt: now}
	if err := s.auditPilot(ctx, tx, "vm.pilot-reserved", pilotState{Execution: current}); err != nil {
		return vmrunner.PilotExecution{}, err
	}
	if err := tx.Commit(); err != nil {
		return vmrunner.PilotExecution{}, err
	}
	return current, nil
}

func (s *Store) PilotExecution(ctx context.Context, runID string) (vmrunner.PilotExecution, error) {
	value, err := scanPilot(s.db.QueryRowContext(ctx, `SELECT `+pilotColumns+` FROM vm_suite_executions WHERE run_id=?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return value, err
}

func (s *Store) PendingPilots(ctx context.Context) ([]vmrunner.PilotExecution, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pilotColumns+` FROM vm_suite_executions WHERE cleaned=0 ORDER BY created_at,run_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var values []vmrunner.PilotExecution
	for rows.Next() {
		value, err := scanPilot(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Store) PilotResources(ctx context.Context, runID string) ([]vmrunner.PilotResource, error) {
	// One read transaction keeps the immutable plan and resource snapshot bound.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	state, err := readPilotState(ctx, tx, runID)
	return state.Resources, err
}

func pilotRunEligible(ctx context.Context, tx *sql.Tx, plan vmrunner.PilotPlan, active bool) error {
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, plan.Spec.RunID))
	if err != nil {
		return err
	}
	var repo string
	var upstream model.Upstream
	if err := tx.QueryRowContext(ctx, `SELECT r.name,u.name,u.kind,u.base_url FROM repositories r JOIN upstreams u ON u.id=r.upstream_id WHERE r.id=?`, run.RepoID).Scan(&repo, &upstream.Name, &upstream.Kind, &upstream.BaseURL); err != nil {
		return err
	}
	if (run.Status != model.RunRunning && (active || run.Status != model.RunPassed)) || run.Credentialed || run.Release || run.TestedSHA != plan.Spec.CandidateSHA || plan.Spec.Repo != upstream.QualifiedRepo(repo) {
		return fmt.Errorf("%w: pilot requires its exact uncredentialed nonrelease run", ErrInvalidState)
	}
	var cancellations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_cancellations WHERE run_id=?`, run.ID).Scan(&cancellations); err != nil {
		return err
	}
	if cancellations != 0 {
		return fmt.Errorf("%w: pilot run has been canceled", ErrInvalidState)
	}
	return nil
}

func scanPilot(row rowScanner) (vmrunner.PilotExecution, error) {
	var value vmrunner.PilotExecution
	var plan, identity, attempt, receipt, endpoint, restart, attach string
	var created, updated int64
	if err := row.Scan(&plan, &identity, &value.Submitted, &value.Cleaning, &value.Cleaned, &value.Failure, &attempt, &receipt, &endpoint, &restart, &attach, &created, &updated); err != nil {
		return value, err
	}
	if err := decodePilotJSON(plan, &value.Plan); err != nil {
		return value, err
	}
	if vmrunner.ValidatePilotPlan(value.Plan) != nil || vmrunner.PilotIdentity(value.Plan) != identity {
		return value, fmt.Errorf("%w: corrupted sealed pilot plan", ErrInvalidState)
	}
	if attempt != "" {
		value.Attempt = new(vmrunner.ConductorAttempt)
		if err := decodePilotJSON(attempt, value.Attempt); err != nil {
			return value, err
		}
		if err := vmrunner.ValidateConductorAttempt(value.Plan, *value.Attempt); err != nil {
			return value, fmt.Errorf("%w: corrupted conductor attempt", ErrInvalidState)
		}
	}
	if attach != "" {
		value.Attach = new(vmrunner.ConductorAttachClaim)
		if err := decodePilotJSON(attach, value.Attach); err != nil {
			return value, err
		}
		if value.Attempt == nil || vmrunner.ValidateConductorAttachClaim(value.Plan, *value.Attempt, *value.Attach) != nil {
			return value, fmt.Errorf("%w: corrupted attach claim", ErrInvalidState)
		}
	}
	if receipt != "" {
		value.Receipt = new(vmrunner.PilotReceipt)
		if err := decodePilotJSON(receipt, value.Receipt); err != nil {
			return value, err
		}
		if value.Attempt == nil || value.Attach == nil || value.Attach.BoundAt.IsZero() || vmrunner.VerifyPilotReceipt(value.Plan, *value.Receipt) != nil || vmrunner.ConductorAttemptIdentity(*value.Attempt) != vmrunner.ConductorAttemptIdentity(value.Receipt.Attempt) {
			return value, fmt.Errorf("%w: corrupted pilot receipt", ErrInvalidState)
		}
	}
	if endpoint != "" {
		value.Endpoint = new(vmrunner.PilotEndpoint)
		if err := decodePilotJSON(endpoint, value.Endpoint); err != nil {
			return value, err
		}
		if vmrunner.ValidatePilotEndpoint(*value.Endpoint) != nil || value.Endpoint.Attempt != vmrunner.InitialVMResource {
			return value, fmt.Errorf("%w: corrupted initial endpoint", ErrInvalidState)
		}
	}
	if restart != "" {
		value.Restart = new(vmrunner.PilotRestart)
		if err := decodePilotJSON(restart, value.Restart); err != nil {
			return value, err
		}
		if value.Attempt == nil || value.Endpoint == nil || vmrunner.ValidatePilotRestartRequest(value.Plan, *value.Attempt, value.Restart.Request) != nil || value.Restart.Request.Old != *value.Endpoint {
			return value, fmt.Errorf("%w: corrupted restart operation", ErrInvalidState)
		}
		if value.Restart.Response != nil && vmrunner.ValidatePilotRestartResponse(value.Restart.Request, *value.Restart.Response) != nil {
			return value, fmt.Errorf("%w: corrupted restart response", ErrInvalidState)
		}
	}
	value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	if value.Attach != nil && (value.Attach.ClaimedAt.Before(value.CreatedAt) ||
		!value.Attach.ClaimedAt.Before(value.CreatedAt.Add(value.Plan.Spec.Deadline)) ||
		(!value.Attach.BoundAt.IsZero() && (value.Attach.RuntimeStartedAt.Before(value.CreatedAt) ||
			!value.Attach.RuntimeStartedAt.Before(value.CreatedAt.Add(value.Plan.Spec.Deadline)) ||
			!value.Attach.BoundAt.Before(value.CreatedAt.Add(value.Plan.Spec.Deadline))))) {
		return value, fmt.Errorf("%w: attach claim exceeds sealed pilot lifetime", ErrInvalidState)
	}
	return value, nil
}

func decodePilotJSON(body string, target any) error {
	if len(body) > 64<<10 || json.Unmarshal([]byte(body), target) != nil {
		return fmt.Errorf("%w: malformed pilot journal", ErrInvalidState)
	}
	canonical, err := json.Marshal(target)
	if err != nil || string(canonical) != body {
		return fmt.Errorf("%w: noncanonical pilot journal", ErrInvalidState)
	}
	return nil
}

func readPilotState(ctx context.Context, tx *sql.Tx, runID string) (pilotState, error) {
	var state pilotState
	var err error
	state.Execution, err = scanPilot(tx.QueryRowContext(ctx, `SELECT `+pilotColumns+` FROM vm_suite_executions WHERE run_id=?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return state, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+pilotResourceColumns+` FROM vm_suite_resources WHERE run_id=? ORDER BY resource_key LIMIT ?`, runID, vmrunner.MaxPilotResources+1)
	if err != nil {
		return state, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var value vmrunner.PilotResource
		var key, intent, receipt, uid string
		if err := rows.Scan(&key, &intent, &receipt, &uid, &value.Submitted, &value.Rejected, &value.Cleaning, &value.Cleaned); err != nil {
			return state, err
		}
		if err := decodePilotJSON(intent, &value.Intent); err != nil {
			return state, err
		}
		if vmrunner.ValidatePilotResource(state.Execution.Plan, value.Intent) != nil || value.Intent.Key != key {
			return state, fmt.Errorf("%w: corrupted pilot resource intent", ErrInvalidState)
		}
		if receipt != "" {
			if err := decodePilotJSON(receipt, &value.Receipt); err != nil {
				return state, err
			}
		}
		if value.Receipt.UID != uid || (uid != "" && value.Receipt.SpecIdentity != value.Intent.SpecIdentity) {
			return state, fmt.Errorf("%w: corrupted pilot resource binding", ErrInvalidState)
		}
		state.Resources = append(state.Resources, value)
	}
	if len(state.Resources) > vmrunner.MaxPilotResources {
		return state, fmt.Errorf("%w: pilot resource bound exceeded", ErrInvalidState)
	}
	return state, rows.Err()
}

// pilotRejection commits a durable failure and then returns ErrInvalidState.
// All other mutation errors roll back, including an unavailable audit chain.
type pilotRejection struct{}

func (pilotRejection) Error() string { return "store: pilot evidence rejected; cleanup required" }
func (pilotRejection) Unwrap() error { return ErrInvalidState }

func poisonPilot(state *pilotState, code string) error {
	if state.Execution.Failure == "" {
		state.Execution.Failure = code
	}
	state.Execution.Cleaning = true
	return pilotRejection{}
}

func (s *Store) changePilot(ctx context.Context, runID, action string, change func(*sql.Tx, *pilotState, time.Time) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	state, err := readPilotState(ctx, tx, runID)
	if err != nil {
		return err
	}
	before, err := json.Marshal(state)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	changeErr := change(tx, &state, now)
	var rejected pilotRejection
	if changeErr != nil && !errors.As(changeErr, &rejected) {
		return changeErr
	}
	slices.SortFunc(state.Resources, func(a, b vmrunner.PilotResource) int {
		if a.Intent.Key < b.Intent.Key {
			return -1
		}
		if a.Intent.Key > b.Intent.Key {
			return 1
		}
		return 0
	})
	after, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if string(before) == string(after) {
		return changeErr
	}
	state.Execution.UpdatedAt = now
	if err := savePilotState(ctx, tx, state); err != nil {
		return err
	}
	if err := s.auditPilot(ctx, tx, action, state); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return changeErr
}

func savePilotState(ctx context.Context, tx *sql.Tx, state pilotState) error {
	value := state.Execution
	var attempt, receipt, endpoint, restart, attach string
	if value.Attempt != nil {
		body, err := json.Marshal(value.Attempt)
		if err != nil {
			return err
		}
		attempt = string(body)
	}
	if value.Receipt != nil {
		body, err := json.Marshal(value.Receipt)
		if err != nil {
			return err
		}
		receipt = string(body)
	}
	if value.Endpoint != nil {
		body, err := json.Marshal(value.Endpoint)
		if err != nil {
			return err
		}
		endpoint = string(body)
	}
	if value.Restart != nil {
		body, err := json.Marshal(value.Restart)
		if err != nil {
			return err
		}
		restart = string(body)
	}
	if value.Attach != nil {
		body, err := json.Marshal(value.Attach)
		if err != nil {
			return err
		}
		attach = string(body)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE vm_suite_executions SET submitted=?,cleaning=?,cleaned=?,failure=?,attempt_json=?,receipt_json=?,endpoint_json=?,restart_json=?,attach_json=?,updated_at=? WHERE run_id=?`, value.Submitted, value.Cleaning, value.Cleaned, value.Failure, attempt, receipt, endpoint, restart, attach, unixNano(value.UpdatedAt), value.Plan.Spec.RunID); err != nil {
		return err
	}
	for _, resource := range state.Resources {
		intent, err := json.Marshal(resource.Intent)
		if err != nil {
			return err
		}
		var binding string
		if resource.Receipt.UID != "" {
			body, err := json.Marshal(resource.Receipt)
			if err != nil {
				return err
			}
			binding = string(body)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO vm_suite_resources(run_id,resource_key,intent_json,receipt_json,uid,submitted,rejected,cleaning,cleaned) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(run_id,resource_key) DO UPDATE SET receipt_json=excluded.receipt_json,uid=excluded.uid,submitted=excluded.submitted,rejected=excluded.rejected,cleaning=excluded.cleaning,cleaned=excluded.cleaned`, value.Plan.Spec.RunID, resource.Intent.Key, string(intent), binding, resource.Receipt.UID, resource.Submitted, resource.Rejected, resource.Cleaning, resource.Cleaned); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) auditPilot(ctx context.Context, tx *sql.Tx, action string, state pilotState) error {
	var actor string
	runID := state.Execution.Plan.Spec.RunID
	if err := tx.QueryRowContext(ctx, `SELECT actor FROM runs WHERE id=?`, runID).Scan(&actor); err != nil {
		return err
	}
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: action, ResourceType: "run", ResourceID: runID, Details: string(body)}, unixNano(state.Execution.UpdatedAt))
	return err
}

func (state *pilotState) resource(key string) *vmrunner.PilotResource {
	for index := range state.Resources {
		if state.Resources[index].Intent.Key == key {
			return &state.Resources[index]
		}
	}
	return nil
}

func (state *pilotState) creating(now time.Time) bool {
	value := state.Execution
	return value.Submitted && !value.Cleaning && !value.Cleaned && value.Failure == "" && now.Before(value.CreatedAt.Add(value.Plan.Spec.Deadline))
}
