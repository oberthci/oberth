package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// resumeOrigin runs inside the accepting BEGIN IMMEDIATE transaction. One
// server process and SQLite's serialized writer give each predecessor one heir.
func resumeOrigin(ctx context.Context, tx *sql.Tx, input stateport.Acceptance) (model.RunOrigin, error) {
	o := *input.Resume
	old, err := loadRun(ctx, tx, o.ResumedFrom)
	if err != nil {
		return o, err
	}
	if !resumeOriginMatches(old, input) {
		return o, refuse(invalid, nil)
	}
	var found int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM runs INDEXED BY run_ref WHERE repo_id=? AND ref=? AND json_extract(origin,'$.resumed_from')=?`, old.RepositoryID, old.Ref, old.ID).Scan(&found)
	if err != nil {
		return o, databaseError(err)
	}
	if found != 0 {
		return o, publicerr.New(publicerr.Conflict, "run is already resumed", "Push the fix with -o oberth.run=1 for a full unassisted run.", nil)
	}
	return o, resumePlanChecks(ctx, tx, input, old)
}

func resumePlanChecks(ctx context.Context, tx *sql.Tx, input stateport.Acceptance, old stateport.Run) error {
	o := input.Resume
	var plan nativebinding.ResumePlan
	if json.Unmarshal(o.ResumePlan, &plan) != nil || !validResumePlan(*o, plan, input.Prepared.View().RunID) {
		return refuse(invalid, nil)
	}
	if plan.InPod == nil && old.Status != stepmodel.RunFailed {
		return refuse(invalid, nil)
	}
	if plan.InPod != nil {
		if err := validateInPodAcceptance(old, input, plan); err != nil {
			return err
		}
	}
	if len(plan.SecretSteps) > 0 {
		if err := resumeInspection(ctx, tx, old.ID); err != nil {
			return err
		}
	}
	return nil
}

// ResumeSuccessor resolves one link through the existing run_ref index.
func (s *Store) ResumeSuccessor(ctx context.Context, id string) (string, error) {
	var next string
	err := s.readers.QueryRowContext(ctx, `SELECT n.id FROM runs AS p JOIN runs AS n INDEXED BY run_ref ON n.repo_id=p.repo_id AND n.ref=p.ref WHERE p.id=? AND json_extract(n.origin,'$.resumed_from')=?`, id, id).Scan(&next)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return next, databaseError(err)
}

func resumeOriginMatches(old stateport.Run, input stateport.Acceptance) bool {
	o := input.Resume
	return old.Origin.NativeUID() != "" && old.Origin.WorkflowName != "" && old.Repo == input.Request.Repo && old.Ref == input.Request.Ref && (old.Status == stepmodel.RunFailed || old.Status == stepmodel.RunRunning && old.Origin.AssistRequested) && old.Origin.NativeUID() == o.NativeUID() && old.Origin.WorkflowName == o.WorkflowName && o.Assisted
}

func resumeAudit(input stateport.Acceptance, run stateport.Run) any {
	var plan nativebinding.ResumePlan
	_ = json.Unmarshal(input.Resume.ResumePlan, &plan)
	detail := map[string]any{"resumed_from": input.Resume.ResumedFrom, "commit": run.CommitSHA, "mode": plan.Mode(), "pods": plan.Pods, "secret_steps": plan.SecretSteps}
	if h := plan.InPod; h != nil {
		detail["export_digest"], detail["pod_uid"], detail["node_id"] = h.Digest, h.PodUID, h.NodeID
		detail["failed_attempt_log"] = map[string]string{"run": run.ID, "node": h.NodeID, "container": "main", "before_marker": nativebinding.RerunMarker(h.Source.Revision.Commit, h.RunID)}
	}
	return detail
}

func validResumePlan(o model.RunOrigin, plan nativebinding.ResumePlan, id string) bool {
	return plan.Workflow != nil && string(plan.UID) == o.NativeUID() && plan.Workflow.Name == o.WorkflowName && plan.Workflow.Labels[nativebinding.RunLabel] == id
}
