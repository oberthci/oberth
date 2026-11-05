package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oberthci/oberth/internal/model"
)

type resourceAttemptBinding struct {
	inventory model.ResourceAttemptInventory
	attempt   model.ResourceAttempt
}

// An unbound Pod or legacy missing template is unknown, rather than a definite
// write conflict. Reader acceptance still requires the complete exact binding.
func (b resourceAttemptBinding) consistentUsage(v model.StepResourceUsage) bool {
	i, a := b.inventory, b.attempt
	return i.RunID == v.RunID && i.WorkflowUID == v.WorkflowUID && i.WorkflowName == v.WorkflowName && i.Namespace == v.Namespace && a.NodeID == v.NodeID && a.Stage == v.Stage && a.Step == v.Step && (v.TemplateName == "" || a.Template == v.TemplateName) && (a.PodUID == "" || a.PodUID == v.PodUID) && (a.Owner == "" || v.FragmentRepository == "" || a.Owner == v.FragmentRepository)
}

func (b resourceAttemptBinding) matchesUsage(v model.StepResourceUsage) bool {
	return b.consistentUsage(v) && b.attempt.PodUID != "" && v.TemplateName != ""
}

func (b resourceAttemptBinding) matchesDisposition(d model.PodResourceDisposition) bool {
	i, a := b.inventory, b.attempt
	return i.RunID == d.RunID && i.WorkflowUID == d.WorkflowUID && i.WorkflowName == d.WorkflowName && i.Namespace == d.Namespace && a.NodeID == d.NodeID && a.PodUID != "" && a.PodUID == d.PodUID && a.Stage == d.Stage && a.Step == d.Step && a.Template == d.Template && (a.Owner == "" || d.Owner == "" || a.Owner == d.Owner)
}

func checkAttemptUsage(ctx context.Context, tx *sql.Tx, i model.ResourceAttemptInventory, a model.ResourceAttempt) error {
	rows, err := tx.QueryContext(ctx, `SELECT observation_json FROM step_resource_usage WHERE run_id=? AND node_id=? LIMIT 4097`, i.RunID, a.NodeID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		count++
		var raw string
		var v model.StepResourceUsage
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		if count > 4096 || json.Unmarshal([]byte(raw), &v) != nil || v.Validate() != nil {
			return ErrInvalid
		}
		if v.IsWorkflowMain() && !(resourceAttemptBinding{i, a}).consistentUsage(v) {
			return ErrInvalid
		}
	}
	return rows.Err()
}

func checkUsageAttempt(ctx context.Context, tx *sql.Tx, v model.StepResourceUsage) error {
	if !v.IsWorkflowMain() {
		return nil
	}
	var manifest, attempt string
	err := tx.QueryRowContext(ctx, `SELECT i.observation_json,a.observation_json FROM resource_attempt_inventories i JOIN resource_attempts a ON a.run_id=i.run_id WHERE i.run_id=? AND a.node_id=?`, v.RunID, v.NodeID).Scan(&manifest, &attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var i model.ResourceAttemptInventory
	var a model.ResourceAttempt
	if json.Unmarshal([]byte(manifest), &i) != nil || json.Unmarshal([]byte(attempt), &a) != nil {
		return ErrInvalid
	}
	i.Attempts = []model.ResourceAttempt{a}
	if i.Validate() != nil || !(resourceAttemptBinding{i, a}).consistentUsage(v) {
		return ErrInvalid
	}
	return nil
}
