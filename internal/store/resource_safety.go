package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/model"
)

func (s *Store) readResourceSafety(ctx context.Context, repoID int64, query, activeQuery string, args []any, out *RunResourceHistory) error {
	var ids []string
	known := map[string]bool{}
	for population, q := range []string{query, activeQuery} {
		rows, err := s.reader().QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			run, err := scanRun(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			count++
			if count > measured.MaxRuns {
				// A bounded terminal history is explicit; an omitted active
				// workflow could already contain a known censor or failed retry.
				out.SafetyIncomplete = out.SafetyIncomplete || population == 1
				break
			}
			ids = append(ids, run.ID)
			known[run.ID] = true
			out.SafetyRuns = append(out.SafetyRuns, run)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
	}
	for _, run := range out.Runs {
		if !known[run.ID] {
			out.SafetyIncomplete = true
		}
	}
	if len(ids) == 0 {
		return nil
	}
	payload, _ := json.Marshal(ids)
	var err error
	out.SafetyObservations, out.SafetyIncomplete, err = safetyObservations(s, ctx, repoID, payload, out.SafetyIncomplete)
	if err != nil {
		return err
	}
	rows, err := s.reader().QueryContext(ctx, `SELECT d.observation_json FROM json_each(?) selected
CROSS JOIN pod_resource_dispositions d ON d.run_id=selected.value
WHERE d.repo_id=? ORDER BY d.run_id,d.node_id,d.pod_uid LIMIT ?`, string(payload), repoID, measured.MaxRows+1)
	if err != nil {
		return err
	}
	podCounts := map[string]int{}
	for rows.Next() {
		var raw string
		var d model.PodResourceDisposition
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return err
		}
		if len(raw) > 3072 || json.Unmarshal([]byte(raw), &d) != nil || d.Validate() != nil {
			_ = rows.Close()
			return fmt.Errorf("invalid stored Pod disposition")
		}
		out.Dispositions = append(out.Dispositions, d)
		podCounts[d.RunID]++
		if podCounts[d.RunID] >= 2048 {
			out.SafetyIncomplete = true
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if len(out.Dispositions) > measured.MaxRows {
		out.SafetyIncomplete = true
		out.Dispositions = out.Dispositions[:measured.MaxRows]
	}
	if err := s.readResourceAttemptSafety(ctx, repoID, payload, out); err != nil {
		return err
	}
	// Executed steps without any usage/disposition must not vanish. We cannot
	// safely reconstruct a missing template or Pod identity from a final step
	// name, so this is an explicit report-wide uncertainty fence.
	if out.SafetyIncomplete {
		return nil
	}
	inventory, err := s.reader().QueryContext(ctx, `SELECT
 r.status<>'skipped' AND (runs.status IN ('passed','failed','interrupted') OR r.status IN ('passed','failed','timed_out'))
 AND NOT EXISTS(SELECT 1 FROM step_resource_usage u WHERE u.run_id=r.run_id AND u.burn=r.burn AND u.step=r.step AND json_extract(u.observation_json,'$.container_name')='main')
 AND NOT EXISTS(SELECT 1 FROM pod_resource_dispositions d WHERE d.run_id=r.run_id AND d.burn=r.burn AND d.step=r.step)
 FROM json_each(?) selected CROSS JOIN step_results r ON r.run_id=selected.value JOIN runs ON runs.id=r.run_id
 LIMIT ?`, string(payload), measured.MaxRows+1)
	if err != nil {
		return err
	}
	count := 0
	for inventory.Next() {
		var missing bool
		if err := inventory.Scan(&missing); err != nil {
			_ = inventory.Close()
			return err
		}
		count++
		if missing || count > measured.MaxRows {
			out.SafetyIncomplete = true
			break
		}
	}
	err = inventory.Err()
	_ = inventory.Close()
	if err != nil {
		return err
	}
	// A terminal run with neither step results nor independent observations is
	// not evidence that no work executed (e.g. an interrupted collector).
	var empty int
	err = s.reader().QueryRowContext(ctx, `SELECT count(*) FROM (
 SELECT 1 FROM json_each(?) selected JOIN runs ON runs.id=selected.value WHERE runs.status IN ('passed','failed','interrupted') AND
 NOT EXISTS(SELECT 1 FROM step_results r WHERE r.run_id=selected.value)
 AND NOT EXISTS(SELECT 1 FROM step_resource_usage u WHERE u.run_id=selected.value)
 AND NOT EXISTS(SELECT 1 FROM pod_resource_dispositions d WHERE d.run_id=selected.value)
 LIMIT 1)`, string(payload)).Scan(&empty)
	out.SafetyIncomplete = out.SafetyIncomplete || empty > 0
	return err
}

func safetyObservations(s *Store, ctx context.Context, repoID int64, payload []byte, incomplete bool) ([]measured.Observation, bool, error) {
	values, truncated, err := s.readResourceObservations(ctx, repoID, payload)
	return values, incomplete || truncated, err
}
