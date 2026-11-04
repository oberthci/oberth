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
	// DurationClass filters workflow history before applying the run limit.
	DurationClass     string
	IncludePromotions bool
	Ref               string
	RunID             string
	Runs              int
	IncludeFailed     bool
}

// Keep the executed query and its EXPLAIN test identical. CROSS JOIN fixes the
// bounded selected-run inventory as the outer loop; INDEXED BY requires each
// observation lookup to use the existing run_id primary-key prefix.
const runResourceObservationSQL = `SELECT u.observation_json,s.termination_json,s.memory_peak_bytes,s.status,r.error,r.status
 FROM json_each(?) selected
 CROSS JOIN step_resource_usage u INDEXED BY sqlite_autoindex_step_resource_usage_1 ON u.run_id=selected.value
 JOIN runs r ON r.id=u.run_id
 LEFT JOIN step_results s ON s.run_id=u.run_id AND s.burn=u.burn AND s.step=u.step
 WHERE u.repo_id=?
 AND json_extract(u.observation_json,'$.container_name')='main'
 AND COALESCE(json_extract(u.observation_json,'$.container_kind'),'app')='app'
 AND COALESCE(json_extract(u.observation_json,'$.pod_kind'),'workflow')='workflow'
 ORDER BY u.last_observed_at DESC,u.run_id,u.node_id,u.pod_uid,u.container_id LIMIT ?`

type RunResourceHistory struct {
	AttemptGaps        []model.ResourceAttemptGap
	SafetyObservations []measured.Observation
	Dispositions       []model.PodResourceDisposition
	SafetyRuns         []model.Run
	SafetyIncomplete   bool
	Runs               []model.Run
	Observations       []measured.Observation
	Truncated          bool
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
		if f.IncludePromotions {
			query += ` AND (ref=? OR (trigger='promotion' AND EXISTS (
 SELECT 1 FROM promotions p WHERE p.run_id=runs.id AND p.run_id!='' AND p.repo_id=? AND p.target_ref=?)))`
			args = append(args, f.Ref, repoID, f.Ref)
		} else {
			query += ` AND ref=?`
			args = append(args, f.Ref)
		}
	}
	switch f.DurationClass {
	case "":
	case "release":
		query += ` AND release=1`
	case "promotion", "schedule":
		query += ` AND release=0 AND trigger=?`
		args = append(args, f.DurationClass)
	case "branch":
		query += ` AND release=0 AND trigger NOT IN ('promotion','schedule')`
	default:
		return out, ErrInvalid
	}
	safetyQuery := query + ` AND status IN ('passed','failed','interrupted') ORDER BY queue_sequence DESC LIMIT ?`
	safetyArgs := append(append([]any(nil), args...), measured.MaxRuns+1)
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
	if err != nil {
		return out, err
	}
	if err = s.readResourceSafety(ctx, repoID, safetyQuery, safetyArgs, &out); err != nil {
		return out, err
	}
	if len(ids) == 0 {
		return out, nil
	}
	payload, err := json.Marshal(ids)
	if err != nil {
		return out, err
	}
	out.Observations, out.Truncated, err = s.readResourceObservations(ctx, repoID, payload)
	return out, err
}

func (s *Store) readResourceObservations(ctx context.Context, repoID int64, payload []byte) ([]measured.Observation, bool, error) {
	var values []measured.Observation
	// The run_id prefix of the primary key bounds each lookup to at most
	// 4,096 retained incarnations; helpers are excluded before the output cap.
	observations, err := s.reader().QueryContext(ctx, runResourceObservationSQL, string(payload), repoID, measured.MaxRows+1)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = observations.Close() }()
	for observations.Next() {
		var raw string
		var diagnostic *string
		var memory *int64
		var status *string
		var runError, runStatus string
		if err := observations.Scan(&raw, &diagnostic, &memory, &status, &runError, &runStatus); err != nil {
			return nil, false, err
		}
		var v model.StepResourceUsage
		if json.Unmarshal([]byte(raw), &v) != nil || v.Validate() != nil {
			return nil, false, fmt.Errorf("invalid stored resource observation")
		}
		o := measured.Observation{RunTerminal: model.RunStatus(runStatus).Terminal(), Usage: v, Template: measured.TemplateOf(v), Owner: v.FragmentRepository}
		o.DeadlineKilled = model.IsExecutionDeadlineReason(runError) || (status != nil && model.StepStatus(*status) == model.StepTimedOut)
		if diagnostic != nil {
			var d model.StepDiagnostic
			if json.Unmarshal([]byte(*diagnostic), &d) == nil {
				stepStatus := model.StepStatus("")
				if status != nil {
					stepStatus = model.StepStatus(*status)
				}
				o = measured.WithStepResult(o, &d, memory, stepStatus)
			}
		}
		values = append(values, o)
	}
	truncated := len(values) > measured.MaxRows
	if truncated {
		values = values[:measured.MaxRows]
	}
	return values, truncated, observations.Err()
}
