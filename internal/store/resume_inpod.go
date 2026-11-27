package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"reflect"
	"slices"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func validateInPodAcceptance(old stateport.Run, input stateport.Acceptance, plan nativebinding.ResumePlan) error {
	h := plan.InPod
	if old.Status != stepmodel.RunRunning || !old.Origin.AssistRequested {
		return refuse(invalid, nil)
	}
	if !validHeldTransfer(*h, input) || h.RunID != input.Prepared.View().RunID {
		return refuse(invalid, nil)
	}
	return validateInPodEntries(old.Admission.Plan, plan)
}

func validHeldTransfer(h nativebinding.ResumeHold, input stateport.Acceptance) bool {
	digest, err := hex.DecodeString(h.Digest)
	if err != nil || len(digest) != 32 || h.PodUID == "" || h.NodeID == "" || h.PodName == "" || h.OriginalExit <= 0 || h.OriginalExit > 255 || h.HeldSince.IsZero() {
		return false
	}
	if h.Source.Revision.Commit != *input.Request.CommitSHA || h.Source.Descriptor.Kind != sourcecontract.Main || h.Source.Path != "/work/src" {
		return false
	}
	return slices.ContainsFunc(input.Prepared.View().SourceBindings, func(b sourcecontract.Binding) bool { return reflect.DeepEqual(b, h.Source) })
}

func validateInPodEntries(authored admissioncontract.Plan, plan nativebinding.ResumePlan) error {
	h := plan.InPod
	held := false
	for _, e := range authored.Entries {
		if !slices.Contains(plan.Carried, e.Path) && !slices.Contains(plan.Remaining, e.Path) {
			return refuse(invalid, nil)
		}
		if !slices.Contains(plan.Remaining, e.Path) {
			continue
		}
		if e.Path == h.Step {
			held = e.HoldArmed && e.HoldEligible && !e.ResultConsumed
		}
	}
	if !held {
		return refuse(invalid, nil)
	}
	return nil
}

// The predecessor's final record and the Assisted successor are committed in
// one transaction. A later native result belongs only to the new owner.
func (s *Store) finishInPodPredecessor(ctx context.Context, tx *sql.Tx, input stateport.Acceptance, next stateport.Run) error {
	var plan nativebinding.ResumePlan
	if err := json.Unmarshal(input.Resume.ResumePlan, &plan); err != nil {
		return err
	}
	if plan.InPod == nil {
		return nil
	}
	old, err := loadRun(ctx, tx, input.Resume.ResumedFrom)
	if err != nil {
		return err
	}
	var stopped int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM obligations WHERE run_id=? AND kind='stop'`, old.ID).Scan(&stopped); err != nil {
		return databaseError(err)
	}
	if stopped != 0 {
		return refuse(terminalRun, nil)
	}
	h := plan.InPod
	if err = recordHeldFailure(ctx, tx, old.ID, *h); err != nil {
		return err
	}
	old.Origin.Assisted = true
	if err = s.saveVerdict(ctx, tx, &old, stateport.RunUpdate{ID: old.ID, Revision: old.Revision, Status: stepmodel.RunFailed, Reason: "resumed in place", Detail: fmt.Sprintf("as %s; %s held after exit %d", next.ID, h.Step, h.OriginalExit)}); err != nil {
		return err
	}
	if err = s.persistHeldProjection(ctx, tx, old, *h); err != nil {
		return err
	}
	return s.heldHandoffEvent(ctx, tx, old)
}

func (s *Store) persistHeldProjection(ctx context.Context, tx *sql.Tx, old stateport.Run, h nativebinding.ResumeHold) error {
	progress, err := s.readProgress(ctx, tx, old.ID)
	if err != nil {
		return err
	}
	var steps []stepmodel.RunStep
	if progress.steps != "" {
		if err = json.Unmarshal([]byte(progress.steps), &steps); err != nil {
			return err
		}
	}
	steps = stateport.PlanSteps(old.Admission.Plan, steps)
	for i := range steps {
		if steps[i].Path == h.Step {
			steps[i].Status, steps[i].FinishedAt = stepmodel.StepFailed, &h.HeldSince
			for j := range steps[i].Attempts {
				if steps[i].Attempts[j].NodeID == h.NodeID {
					steps[i].Attempts[j].Status = stepmodel.StepFailed
				}
			}
		}
	}
	old.LogicalStepCounts, err = stepmodel.CountSteps(steps, old.Admission.Plan.Exact)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO native_steps VALUES(?,?) ON CONFLICT(run_id) DO UPDATE SET value=excluded.value`, old.ID, encode(steps)); err != nil {
		return databaseError(err)
	}
	return s.writeProgressSummary(ctx, tx, old.ID, old.RunSummary)
}

func (s *Store) heldHandoffEvent(ctx context.Context, tx *sql.Tx, old stateport.Run) error {
	data := json.RawMessage(nil)
	if old.Origin.Resumed() {
		data, _ = json.Marshal(map[string]string{"resumed_from": old.Origin.ResumedFrom})
	}
	_, err := s.insertEvent(ctx, tx, stateport.EventWrite{Event: model.AgentEvent{Kind: string(model.EventRunFinished), At: s.options.Clock(), Repo: old.Repo, RunID: old.ID, Ref: old.Ref, ObjectSHA: old.ObjectSHA, CommitSHA: old.CommitSHA, Summary: "resumed in place", Data: data}, Dedupe: "run.finished:" + old.ID + ":in-pod"})
	return err
}

func recordHeldFailure(ctx context.Context, tx *sql.Tx, id string, h nativebinding.ResumeHold) error {
	nodes, err := readNodes(ctx, tx, id)
	if err != nil {
		return err
	}
	n := stateport.Node{NodeID: h.NodeID, Path: h.Step, PodName: h.PodName, PodUID: h.PodUID, Attempt: 1, Credential: new(false)}
	for _, saved := range nodes {
		if saved.NodeID == h.NodeID {
			n = saved
			break
		}
	}
	if n.PodUID != "" && n.PodUID != h.PodUID {
		return refuse(nodeBinding, nil)
	}
	n.PodUID, n.PodName, n.Status, n.Exit, n.FinishedAt = h.PodUID, h.PodName, stepmodel.StepFailed, new(h.OriginalExit), &h.HeldSince
	_, err = tx.ExecContext(ctx, `INSERT INTO nodes VALUES(?,?,?,?,?,?) ON CONFLICT(run_id,node_id) DO UPDATE SET value=excluded.value`, id, n.NodeID, n.Path, false, encode(n), 0)
	return databaseError(err)
}

// Retained native evidence may ordinarily settle after a terminal record.
// A handed-off predecessor is final: its old tracker cannot overwrite it.
func requireUnhandedRun(ctx context.Context, tx *sql.Tx, id string) error {
	var frozen bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runs p JOIN runs n INDEXED BY run_ref ON n.repo_id=p.repo_id AND n.ref=p.ref WHERE p.id=? AND p.status='failed' AND json_extract(n.origin,'$.resumed_from')=p.id AND json_extract(n.origin,'$.resume_plan.InPod') IS NOT NULL)`, id).Scan(&frozen)
	if err != nil {
		return databaseError(err)
	}
	if frozen {
		return refuse(terminalRun, nil)
	}
	return nil
}
