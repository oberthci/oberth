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

// RecordStepResourceUsage accepts only server-side observations, never an HTTP
// ingestion payload. One bounded transaction amortizes telemetry writer cost.
func (s *Store) RecordStepResourceUsage(ctx context.Context, values []model.StepResourceUsage) error {
	if len(values) > 256 {
		return fmt.Errorf("%w: resource observation batch exceeds bound", ErrInvalid)
	}
	if len(values) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, v := range values {
		if v.Validate() != nil {
			return fmt.Errorf("%w: invalid resource observation", ErrInvalid)
		}
		var repoID int64
		var jobName string
		if err := tx.QueryRowContext(ctx, `SELECT repo_id,job_name FROM runs WHERE id=?`, v.RunID).Scan(&repoID, &jobName); err != nil {
			return err
		}
		if jobName != v.WorkflowName {
			return fmt.Errorf("%w: resource observation job binding mismatch", ErrInvalid)
		}
		var previous string
		err := tx.QueryRowContext(ctx, `SELECT observation_json FROM step_resource_usage WHERE run_id=? AND node_id=? AND pod_uid=? AND container_id=?`, v.RunID, v.NodeID, v.PodUID, v.ContainerID).Scan(&previous)
		if err == nil {
			var old model.StepResourceUsage
			if json.Unmarshal([]byte(previous), &old) != nil {
				return errors.New("invalid stored resource observation")
			}
			v, err = model.MergeResourceUsage(old, v)
			if err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		} else {
			// Keep existing incarnations updatable after the storage bound is reached.
			// Run/report views explicitly do not promise population-complete coverage.
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM step_resource_usage WHERE run_id=?`, v.RunID).Scan(&count); err != nil {
				return err
			}
			if count >= 4096 {
				continue
			}
		}
		body, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if string(body) == previous {
			continue
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO step_resource_usage(run_id,repo_id,node_id,pod_uid,container_id,burn,step,last_observed_at,observation_json) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(run_id,node_id,pod_uid,container_id) DO UPDATE SET last_observed_at=excluded.last_observed_at,observation_json=excluded.observation_json`, v.RunID, repoID, v.NodeID, v.PodUID, v.ContainerID, v.Burn, v.Step, unixNano(v.LastObservedAt), string(body))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) StepResourceUsage(ctx context.Context, runID string) ([]model.StepResourceUsage, bool, error) {
	rows, err := s.reader().QueryContext(ctx, `SELECT observation_json FROM step_resource_usage WHERE run_id=? ORDER BY node_id,pod_uid,container_id LIMIT 257`, runID)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	out, err := readResourceUsage(rows)
	if err != nil {
		return nil, false, err
	}
	more := len(out) > 256
	if more {
		out = out[:256]
	}
	return out, more, nil
}

func readResourceUsage(rows *sql.Rows) ([]model.StepResourceUsage, error) {
	out := []model.StepResourceUsage{}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var v model.StepResourceUsage
		if json.Unmarshal([]byte(body), &v) != nil || v.Validate() != nil {
			return nil, errors.New("invalid stored resource observation")
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type ResourceUsageFilter struct {
	Since      time.Time
	Limit      int
	Burn, Step string
}

func (s *Store) RepositoryResourceUsage(ctx context.Context, repoID int64, f ResourceUsageFilter) ([]model.StepResourceUsage, bool, error) {
	if f.Limit == 0 {
		f.Limit = 1000
	}
	if f.Limit < 1 || f.Limit > 5000 || len(f.Burn) > 253 || len(f.Step) > 253 {
		return nil, false, fmt.Errorf("%w: resource report bounds", ErrInvalid)
	}
	where := `repo_id=? AND last_observed_at>=?`
	args := []any{repoID, unixNano(f.Since)}
	if f.Burn != "" {
		where += ` AND burn=?`
		args = append(args, f.Burn)
	}
	if f.Step != "" {
		where += ` AND step=?`
		args = append(args, f.Step)
	}
	args = append(args, f.Limit+1)
	rows, err := s.reader().QueryContext(ctx, `SELECT observation_json FROM step_resource_usage WHERE `+where+` ORDER BY last_observed_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	out, err := readResourceUsage(rows)
	if err != nil {
		return nil, false, err
	}
	more := len(out) > f.Limit
	if more {
		out = out[:f.Limit]
	}
	return out, more, nil
}
