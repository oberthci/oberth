package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
)

// RecordRunTimingMilestone records first observation only. It never infers a
// phase from the historically overloaded runs.started_at column.
func (s *Store) RecordRunTimingMilestone(ctx context.Context, runID, phase string) error {
	query := ""
	switch phase {
	case "checkout-ready":
		query = `UPDATE run_timings SET checkout_ready_at=COALESCE(checkout_ready_at,?) WHERE run_id=?`
	case "source-seed-started":
		query = `UPDATE run_timings SET source_seed_started_at=COALESCE(source_seed_started_at,?) WHERE run_id=?`
	case "source-ready":
		query = `UPDATE run_timings SET source_ready_at=COALESCE(source_ready_at,?) WHERE run_id=?`
	default:
		return fmt.Errorf("%w: unknown timing milestone", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO run_timings(run_id,version) SELECT id,1 FROM runs WHERE id=?`, runID); err != nil {
		return err
	}
	// #nosec G202 -- query is selected from the fixed statements above.
	result, err := tx.ExecContext(ctx, query, unixNano(s.now()), runID)
	if err != nil {
		return err
	}
	if err := requireChanged("run timing", result); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordWorkflowTiming(ctx context.Context, value model.WorkflowTiming) error {
	if err := value.Validate(); err != nil {
		return fmt.Errorf("%w: invalid Workflow timing", ErrInvalid)
	}
	body, _ := json.Marshal(value)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var name string
	if err := tx.QueryRowContext(ctx, `SELECT job_name FROM runs WHERE id=?`, value.RunID).Scan(&name); err != nil {
		return err
	}
	if name != value.Name {
		return fmt.Errorf("%w: Workflow timing run binding mismatch", ErrInvalidState)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO run_timings(run_id,version) VALUES(?,1)`, value.RunID); err != nil {
		return err
	}
	var existing sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT workflow_json FROM run_timings WHERE run_id=?`, value.RunID).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if existing.Valid && existing.String != string(body) {
		var old model.WorkflowTiming
		if json.Unmarshal([]byte(existing.String), &old) == nil {
			legacy := value
			previous, _ := json.Marshal(old)
			candidate, _ := json.Marshal(legacy)
			if string(previous) == string(candidate) {
				return tx.Commit()
			}
		}
		return fmt.Errorf("%w: Workflow timing attempt changed", ErrInvalidState)
	}
	if !existing.Valid {
		_, err = tx.ExecContext(ctx, `UPDATE run_timings SET workflow_json=?,workflow_created_at=? WHERE run_id=?`, string(body), unixNano(value.CreatedAt), value.RunID)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RecordStepTimings(ctx context.Context, values []model.StepTiming) error {
	if len(values) > model.MaxTimingTasks {
		return fmt.Errorf("%w: too many step timings", ErrInvalid)
	}
	if len(values) == 0 {
		return ctx.Err()
	}
	// Validation includes JSON encoding and size checks. Do that CPU work
	// before reserving SQLite's single writer, including for invalid batches.
	bodies := make([]string, len(values))
	for i, value := range values {
		if value.Validate() != nil {
			return fmt.Errorf("%w: invalid step timing", ErrInvalid)
		}
		body, _ := json.Marshal(value)
		bodies[i] = string(body)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// A Workflow graph can have 2048 tasks. Decoding and validating that graph
	// for every task made a batch quadratic while holding the write lock.
	// Cache only inside this transaction, preserving the atomic binding checks.
	type workflowBinding struct {
		UID, Name, Namespace string
	}
	workflows := make(map[string]workflowBinding)
	counts := make(map[string]int)
	for i, value := range values {
		workflow, found := workflows[value.RunID]
		if !found {
			var workflowJSON sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT workflow_json FROM run_timings WHERE run_id=?`, value.RunID).Scan(&workflowJSON)
			if err != nil {
				return err
			}
			var decoded model.WorkflowTiming
			if !workflowJSON.Valid || json.Unmarshal([]byte(workflowJSON.String), &decoded) != nil || decoded.Validate() != nil {
				return fmt.Errorf("%w: step timing Workflow binding mismatch", ErrInvalidState)
			}
			workflow = workflowBinding{UID: decoded.UID, Name: decoded.Name, Namespace: decoded.Namespace}
			workflows[value.RunID] = workflow
		}
		if workflow.UID != value.WorkflowUID || workflow.Name != value.WorkflowName || workflow.Namespace != value.Namespace {
			return fmt.Errorf("%w: step timing Workflow binding mismatch", ErrInvalidState)
		}
		var oldJSON string
		var err error
		err = tx.QueryRowContext(ctx, `SELECT timing_json FROM step_timings WHERE run_id=? AND node_id=?`, value.RunID, value.NodeID).Scan(&oldJSON)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			var old model.StepTiming
			if decodeTiming(oldJSON, &old) != nil || old.Validate() != nil {
				return fmt.Errorf("%w: corrupt step timing", ErrInvalidState)
			}
			if old.WorkflowUID != value.WorkflowUID || old.Namespace != value.Namespace || old.WorkflowName != value.WorkflowName || (old.PodName != "" && value.PodName != "" && old.PodName != value.PodName) || old.Stage != value.Stage || old.Step != value.Step || old.Ordinal != value.Ordinal || (old.PodUID != "" && old.PodUID != value.PodUID && value.PodUID != "") {
				return fmt.Errorf("%w: step timing attempt changed", ErrInvalidState)
			}
			if old.Available {
				if !value.Available {
					continue
				}
				value.ObservedAt = old.ObservedAt
				body, _ := json.Marshal(value)
				if string(body) != oldJSON {
					return fmt.Errorf("%w: completed step timing changed", ErrInvalidState)
				}
				continue
			}
			if !value.Available {
				continue
			}
		} else {
			count, found := counts[value.RunID]
			if !found {
				err = tx.QueryRowContext(ctx, `SELECT count(*) FROM step_timings WHERE run_id=?`, value.RunID).Scan(&count)
				if err != nil {
					return err
				}
			}
			if count >= model.MaxTimingTasks {
				return fmt.Errorf("%w: step timing attempt bound exceeded", ErrInvalidState)
			}
			counts[value.RunID] = count + 1
		}
		available := 0
		if value.Available {
			available = 1
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO step_timings(run_id,node_id,workflow_uid,pod_uid,available,ordinal,timing_json)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(run_id,node_id) DO UPDATE SET pod_uid=excluded.pod_uid,available=excluded.available,timing_json=excluded.timing_json`, value.RunID, value.NodeID, value.WorkflowUID, value.PodUID, available, value.Ordinal, bodies[i])
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RunTiming(ctx context.Context, id string) (model.RunTiming, error) {
	run, err := s.Run(ctx, id)
	if err != nil {
		return model.RunTiming{}, err
	}
	value := model.RunTiming{Version: model.TimingVersion, RunID: id, QueuedAt: run.QueuedAt, Steps: []model.StepTiming{}}
	var claimed, checkout, seed, source, created, terminal sql.NullInt64
	var workflow sql.NullString
	err = s.reader().QueryRowContext(ctx, `SELECT claimed_at,checkout_ready_at,source_seed_started_at,source_ready_at,workflow_created_at,terminal_at,workflow_json FROM run_timings WHERE run_id=?`, id).Scan(&claimed, &checkout, &seed, &source, &created, &terminal, &workflow)
	if errors.Is(err, sql.ErrNoRows) {
		value.Unavailable = "not-recorded"
		return value, nil
	}
	if err != nil {
		return value, err
	}
	// PruneRunTiming keeps the milestones and drops the Workflow and step
	// records; a Workflow creation time without its record marks that.
	pruned := created.Valid && !workflow.Valid
	value.ClaimedAt, value.CheckoutReadyAt, value.SourceSeedStartedAt, value.SourceReadyAt, value.WorkflowCreatedAt, value.TerminalAt = nullableTime(claimed), nullableTime(checkout), nullableTime(seed), nullableTime(source), nullableTime(created), nullableTime(terminal)
	if pruned {
		value.Unavailable = "pruned"
		workflow = sql.NullString{}
	}
	if workflow.Valid {
		var binding model.WorkflowTiming
		if len(workflow.String) > 256<<10 || decodeTiming(workflow.String, &binding) != nil || binding.Validate() != nil || binding.RunID != id || binding.Name != run.WorkflowName {
			return value, fmt.Errorf("%w: corrupt Workflow timing", ErrInvalidState)
		}
		value.Workflow = &binding
	}

	if pruned {
		return value, nil
	}
	rows, err := s.reader().QueryContext(ctx, `SELECT node_id,workflow_uid,pod_uid,available,ordinal,timing_json FROM step_timings WHERE run_id=? ORDER BY ordinal,node_id`, id)
	if err != nil {
		return value, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw, nodeID, workflowUID, podUID string
		var available, ordinal int
		var step model.StepTiming
		if err := rows.Scan(&nodeID, &workflowUID, &podUID, &available, &ordinal, &raw); err != nil {
			return value, err
		}
		if len(raw) > model.MaxStepTimingBytes || decodeTiming(raw, &step) != nil || step.Validate() != nil || step.RunID != id || value.Workflow == nil ||
			step.WorkflowUID != value.Workflow.UID || step.WorkflowName != value.Workflow.Name || step.Namespace != value.Workflow.Namespace ||
			step.NodeID != nodeID || step.WorkflowUID != workflowUID || step.PodUID != podUID || step.Ordinal != ordinal || step.Available != (available == 1) {
			return value, fmt.Errorf("%w: corrupt step timing", ErrInvalidState)
		}
		value.Steps = append(value.Steps, step)
	}
	return value, rows.Err()
}

func decodeTiming(raw string, value any) error {
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	canonical, err := json.Marshal(value)
	if err != nil || string(canonical) != raw {
		return errors.New("noncanonical timing record")
	}
	return nil
}
