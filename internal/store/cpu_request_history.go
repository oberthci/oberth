package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

const cpuRequestHistorySQL = `WITH recent AS MATERIALIZED (
 SELECT id,status,release,trigger FROM runs INDEXED BY runs_repo_started_idx
 WHERE repo_id=? AND started_at IS NOT NULL ORDER BY started_at DESC LIMIT 128
)
 SELECT u.observation_json FROM recent r
 CROSS JOIN step_resource_usage u INDEXED BY sqlite_autoindex_step_resource_usage_1 ON u.run_id=r.id
 WHERE r.status='passed' AND CASE WHEN r.release=1 THEN 'release' WHEN r.trigger IN ('promotion','schedule') THEN r.trigger ELSE 'branch' END=?
 AND json_extract(u.observation_json,'$.container_name')='main'
 AND COALESCE(json_extract(u.observation_json,'$.container_kind'),'app')='app'
 AND COALESCE(json_extract(u.observation_json,'$.pod_kind'),'workflow')='workflow'
 ORDER BY u.last_observed_at DESC LIMIT 4097`

func (s *Store) CPURequestHistory(ctx context.Context, repoID int64, class string) ([]model.StepResourceUsage, error) {
	if repoID <= 0 || (class != "branch" && class != "release" && class != "promotion" && class != "schedule") {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rows, err := s.reader().QueryContext(ctx, cpuRequestHistorySQL, repoID, class)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.StepResourceUsage
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var v model.StepResourceUsage
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, err
		}
		out = append(out, v)
		if len(out) > 4096 {
			return nil, nil
		}
	}
	return out, rows.Err()
}
