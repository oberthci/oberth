package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/model"
)

func (s *Store) readResourceAttemptSafety(ctx context.Context, repoID int64, payload []byte, out *RunResourceHistory) error {
	terminal := map[string]bool{}
	for _, r := range out.SafetyRuns {
		terminal[r.ID] = r.Status.Terminal()
	}
	manifests := map[string]model.ResourceAttemptInventory{}
	rows, err := s.reader().QueryContext(ctx, `SELECT i.observation_json FROM json_each(?) selected CROSS JOIN resource_attempt_inventories i ON i.run_id=selected.value WHERE i.repo_id=? LIMIT ?`, string(payload), repoID, 2*measured.MaxRuns+1)
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw string
		var i model.ResourceAttemptInventory
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return err
		}
		if len(raw) > 1024 || json.Unmarshal([]byte(raw), &i) != nil || i.Validate() != nil {
			_ = rows.Close()
			return fmt.Errorf("invalid stored attempt inventory")
		}
		manifests[i.RunID] = i
		out.SafetyIncomplete = out.SafetyIncomplete || !i.Complete || (terminal[i.RunID] && !i.Terminal)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	// Legacy complete metric rows cannot prove that a superseded attempt was
	// inventoried. Preserve numbers, but do not fabricate attempt coverage.
	for _, o := range out.SafetyObservations {
		if _, ok := manifests[o.Usage.RunID]; !ok {
			out.SafetyIncomplete = true
		}
	}
	for _, d := range out.Dispositions {
		if _, ok := manifests[d.RunID]; !ok {
			out.SafetyIncomplete = true
		}
	}
	usage := map[string][]model.StepResourceUsage{}
	for _, o := range out.SafetyObservations {
		v := o.Usage
		k := v.RunID + "\x00" + v.NodeID
		usage[k] = append(usage[k], v)
	}
	rows, err = s.reader().QueryContext(ctx, `SELECT a.run_id,a.observation_json FROM json_each(?) selected CROSS JOIN resource_attempts a ON a.run_id=selected.value LIMIT ?`, string(payload), measured.MaxRows+1)
	if err != nil {
		return err
	}
	count := 0
	perRun := map[string]int{}
	known := map[string]resourceAttemptBinding{}
	for rows.Next() {
		var runID, raw string
		var a model.ResourceAttempt
		if err := rows.Scan(&runID, &raw); err != nil {
			_ = rows.Close()
			return err
		}
		count++
		perRun[runID]++
		if count > measured.MaxRows || perRun[runID] >= 4096 {
			out.SafetyIncomplete = true
			break
		}
		i, ok := manifests[runID]
		if !ok || len(raw) > 2048 || json.Unmarshal([]byte(raw), &a) != nil {
			_ = rows.Close()
			return fmt.Errorf("invalid stored resource attempt")
		}
		i.Attempts = []model.ResourceAttempt{a}
		if i.Validate() != nil {
			_ = rows.Close()
			return fmt.Errorf("invalid stored resource attempt binding")
		}
		binding := resourceAttemptBinding{i, a}
		known[runID+"\x00"+a.NodeID] = binding
		if !a.Status.Terminal() && !terminal[runID] && !i.Terminal {
			continue
		}
		covered := false
		for _, v := range usage[runID+"\x00"+a.NodeID] {
			if binding.matchesUsage(v) && v.Terminal && !v.ContainerFinishedAt.IsZero() {
				covered = true
			}
		}
		if a.Status != model.StepPassed || !covered {
			out.AttemptGaps = append(out.AttemptGaps, model.ResourceAttemptGap{RunID: runID, WorkflowUID: i.WorkflowUID, NodeID: a.NodeID, Stage: a.Stage, Template: a.Template, Owner: a.Owner})
		}
	}
	err = rows.Err()
	_ = rows.Close()
	for _, o := range out.SafetyObservations {
		binding, ok := known[o.Usage.RunID+"\x00"+o.Usage.NodeID]
		if !ok || !binding.matchesUsage(o.Usage) {
			out.SafetyIncomplete = true
		}
	}
	for _, d := range out.Dispositions {
		binding, ok := known[d.RunID+"\x00"+d.NodeID]
		if !ok || !binding.matchesDisposition(d) {
			out.SafetyIncomplete = true
		}
	}
	return err
}
