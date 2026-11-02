package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/model"
)

type RunResourceFilter struct {
	Ref           string
	RunID         string
	Runs          int
	IncludeFailed bool
}
type RunResourceHistory struct {
	Runs         []model.Run
	Observations []measured.Observation
	Truncated    bool
}

// RunResources uses the read pool, no write transaction, and the existing
// repo/ref and run-primary-key indexes. Both run and incarnation reads are
// bounded. Rare releases have no protected reservation in telemetry retention.
func (s *Store) RunResources(ctx context.Context, repoID int64, f RunResourceFilter) (RunResourceHistory, error) {
	var out RunResourceHistory
	if repoID <= 0 || f.Runs < 1 || f.Runs > measured.MaxRuns || len(f.Ref) > 1024 {
		return out, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	query := `SELECT ` + runColumns + ` FROM runs WHERE repo_id=?`
	args := []any{repoID}
	if f.RunID != "" {
		query += ` AND id=?`
		args = append(args, f.RunID)
	} else if f.Ref != "any" {
		query += ` AND ref=?`
		args = append(args, f.Ref)
	}
	if !f.IncludeFailed {
		query += ` AND status='passed'`
	}
	query += ` ORDER BY queue_sequence DESC LIMIT ?`
	args = append(args, f.Runs)
	rows, err := s.reader().QueryContext(ctx, query, args...)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	ids := []string{}
	for rows.Next() {
		run, e := scanRun(rows)
		if e != nil {
			_ = rows.Close()
			return out, e
		}
		out.Runs = append(out.Runs, run)
		ids = append(ids, run.ID)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil || len(ids) == 0 {
		return out, err
	}
	payload, err := json.Marshal(ids)
	if err != nil {
		return out, err
	}
	// The run_id prefix of the primary key bounds each lookup to at most
	// 4,096 retained incarnations; helpers are excluded before the output cap.
	observations, err := s.reader().QueryContext(ctx, `SELECT u.observation_json,s.termination_json
 FROM step_resource_usage u LEFT JOIN step_results s ON s.run_id=u.run_id AND s.burn=u.burn AND s.step=u.step
 WHERE u.repo_id=? AND u.run_id IN (SELECT value FROM json_each(?))
 AND json_extract(u.observation_json,'$.container_name')='main'
 AND COALESCE(json_extract(u.observation_json,'$.container_kind'),'app')='app'
 AND COALESCE(json_extract(u.observation_json,'$.pod_kind'),'workflow')='workflow'
 ORDER BY u.last_observed_at DESC,u.run_id,u.node_id,u.pod_uid,u.container_id LIMIT ?`, repoID, string(payload), measured.MaxRows+1)
	if err != nil {
		return out, err
	}
	defer func() { _ = observations.Close() }()
	for observations.Next() {
		var raw string
		var diagnostic *string
		if err := observations.Scan(&raw, &diagnostic); err != nil {
			return out, err
		}
		var v model.StepResourceUsage
		if json.Unmarshal([]byte(raw), &v) != nil || v.Validate() != nil {
			return out, fmt.Errorf("invalid stored resource observation")
		}
		o := measured.Observation{Usage: v, Template: measured.TemplateOf(v), Owner: v.FragmentRepository}
		if diagnostic != nil {
			var d model.StepDiagnostic
			if json.Unmarshal([]byte(*diagnostic), &d) == nil {
				o = measured.WithDiagnostic(o, &d)
			}
		}
		out.Observations = append(out.Observations, o)
	}
	out.Truncated = len(out.Observations) > measured.MaxRows
	if out.Truncated {
		out.Observations = out.Observations[:measured.MaxRows]
	}
	return out, observations.Err()
}
