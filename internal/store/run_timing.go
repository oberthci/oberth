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

// RecordAdmissionRequested starts a new episode only after a prior grant was
// parked. Repeated observations and owner resumes do not rewrite an episode.
func (s *Store) RecordAdmissionRequested(ctx context.Context, runID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE id=? AND status='running'`, runID).Scan(&active); err != nil {
		return err
	}
	if active != 1 {
		return fmt.Errorf("%w: timing run is not active", ErrInvalidState)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO run_timings(run_id,version) VALUES(?,1)`, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_admission_timings(run_id,episode,requested_at)
SELECT ?,COALESCE(MAX(episode),0)+1,? FROM run_admission_timings WHERE run_id=?
HAVING NOT EXISTS(SELECT 1 FROM run_admission_timings WHERE run_id=? AND closed_at IS NULL)`, runID, unixNano(s.now()), runID, runID); err != nil {
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
	if err := tx.QueryRowContext(ctx, `SELECT workflow_json FROM run_timings WHERE run_id=?`, value.RunID).Scan(&existing); err != nil {
		return err
	}
	if existing.Valid && existing.String != string(body) {
		return fmt.Errorf("%w: Workflow timing attempt changed", ErrInvalidState)
	}
	if !existing.Valid {
		if _, err := tx.ExecContext(ctx, `UPDATE run_timings SET workflow_json=?,workflow_created_at=? WHERE run_id=?`, string(body), unixNano(value.CreatedAt), value.RunID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RecordStepTimings(ctx context.Context, values []model.StepTiming) error {
	if len(values) > model.MaxTimingTasks {
		return fmt.Errorf("%w: too many step timings", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, value := range values {
		if value.Validate() != nil {
			return fmt.Errorf("%w: invalid step timing", ErrInvalid)
		}
		var workflowJSON sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT workflow_json FROM run_timings WHERE run_id=?`, value.RunID).Scan(&workflowJSON); err != nil {
			return err
		}
		var workflow model.WorkflowTiming
		if !workflowJSON.Valid || json.Unmarshal([]byte(workflowJSON.String), &workflow) != nil || workflow.Validate() != nil || workflow.UID != value.WorkflowUID || workflow.Name != value.WorkflowName || workflow.Namespace != value.Namespace {
			return fmt.Errorf("%w: step timing Workflow binding mismatch", ErrInvalidState)
		}
		var oldJSON string
		err := tx.QueryRowContext(ctx, `SELECT timing_json FROM step_timings WHERE run_id=? AND node_id=?`, value.RunID, value.NodeID).Scan(&oldJSON)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			var old model.StepTiming
			if decodeTiming(oldJSON, &old) != nil || old.Validate() != nil {
				return fmt.Errorf("%w: corrupt step timing", ErrInvalidState)
			}
			if old.WorkflowUID != value.WorkflowUID || old.Namespace != value.Namespace || old.WorkflowName != value.WorkflowName || old.PodName != value.PodName || old.Burn != value.Burn || old.Step != value.Step || old.Ordinal != value.Ordinal || (old.PodUID != "" && old.PodUID != value.PodUID && value.PodUID != "") {
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
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM step_timings WHERE run_id=?`, value.RunID).Scan(&count); err != nil {
				return err
			}
			if count >= model.MaxTimingTasks {
				return fmt.Errorf("%w: step timing attempt bound exceeded", ErrInvalidState)
			}
		}
		body, _ := json.Marshal(value)
		available := 0
		if value.Available {
			available = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO step_timings(run_id,node_id,workflow_uid,pod_uid,available,ordinal,timing_json)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(run_id,node_id) DO UPDATE SET pod_uid=excluded.pod_uid,available=excluded.available,timing_json=excluded.timing_json`, value.RunID, value.NodeID, value.WorkflowUID, value.PodUID, available, value.Ordinal, string(body)); err != nil {
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
	value := model.RunTiming{Version: model.TimingVersion, RunID: id, QueuedAt: run.QueuedAt, Admissions: []model.AdmissionTiming{}, Steps: []model.StepTiming{}}
	var claimed, checkout, seed, source, created, terminal sql.NullInt64
	var workflow sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT claimed_at,checkout_ready_at,source_seed_started_at,source_ready_at,workflow_created_at,terminal_at,workflow_json FROM run_timings WHERE run_id=?`, id).Scan(&claimed, &checkout, &seed, &source, &created, &terminal, &workflow)
	if errors.Is(err, sql.ErrNoRows) {
		value.Unavailable = "not-recorded"
		return value, nil
	}
	if err != nil {
		return value, err
	}
	value.ClaimedAt, value.CheckoutReadyAt, value.SourceSeedStartedAt, value.SourceReadyAt, value.WorkflowCreatedAt, value.TerminalAt = nullableTime(claimed), nullableTime(checkout), nullableTime(seed), nullableTime(source), nullableTime(created), nullableTime(terminal)
	if workflow.Valid {
		var binding model.WorkflowTiming
		if len(workflow.String) > 256<<10 || decodeTiming(workflow.String, &binding) != nil || binding.Validate() != nil || binding.RunID != id || binding.Name != run.JobName {
			return value, fmt.Errorf("%w: corrupt Workflow timing", ErrInvalidState)
		}
		value.Workflow = &binding
	}
	rows, err := s.db.QueryContext(ctx, `SELECT episode,requested_at,granted_at,closed_at FROM run_admission_timings WHERE run_id=? ORDER BY episode`, id)
	if err != nil {
		return value, err
	}
	for rows.Next() {
		var episode model.AdmissionTiming
		var requested, granted, closed sql.NullInt64
		if err := rows.Scan(&episode.Episode, &requested, &granted, &closed); err != nil {
			_ = rows.Close()
			return value, err
		}
		episode.RequestedAt, episode.GrantedAt, episode.ClosedAt = nullableTime(requested), nullableTime(granted), nullableTime(closed)
		value.Admissions = append(value.Admissions, episode)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return value, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT node_id,workflow_uid,pod_uid,available,ordinal,timing_json FROM step_timings WHERE run_id=? ORDER BY ordinal,node_id`, id)
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
