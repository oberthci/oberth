package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

// BindWorkflow records the admitted native name/UID exactly once.
func (s *Store) BindWorkflow(ctx context.Context, action stateport.Action, binding nativebinding.RunBinding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	return s.change(ctx, mutation{action.Actor, "run.bind", "run", binding.RunID, binding}, func(tx *sql.Tx) error {
		r, err := loadRun(ctx, tx, binding.RunID)
		if err != nil {
			return err
		}
		if !r.HasAdmission {
			return refuse(noAdmission, nil)
		}
		if err := requireRunBinding(r, binding, nativebinding.RunBinding{RepositoryID: r.RepositoryID}); err != nil {
			return err
		}
		if r.Origin.WorkflowUID != "" {
			if r.Origin.WorkflowUID != binding.Workflow.UID || r.Origin.WorkflowName != binding.Workflow.Name {
				return refuse(bindingChanged, nil)
			}
			return unchanged{}
		}
		if terminal(r.Status) {
			return refuse(terminalRun, nil)
		}
		r.Origin.WorkflowName = binding.Workflow.Name
		r.Origin.WorkflowUID = binding.Workflow.UID
		_, err = tx.ExecContext(ctx, `UPDATE runs SET origin=?,revision=revision+1 WHERE id=?`, encode(r.Origin), r.ID)
		return databaseError(err)
	}, action.Events...)
}

// Finish is an exact revision update; pending publication owns its settlement.
func (s *Store) Finish(ctx context.Context, action stateport.Action, update stateport.RunUpdate) error {
	if err := validateRunUpdate(update); err != nil {
		return err
	}
	return s.observe(ctx, func(tx *sql.Tx) error {
		r, err := loadRun(ctx, tx, update.ID)
		if err != nil {
			return err
		}
		if err := s.saveVerdict(ctx, tx, &r, update); err != nil {
			return err
		}
		if err := resolveRemainders(ctx, tx, r.ID); err != nil {
			return err
		}
		if update.ProjectCI {
			if err := s.projectVerdict(ctx, tx, r); err != nil {
				return err
			}
		}
		if err := s.finishPromotionFact(ctx, tx, action, update); err != nil {
			return err
		}
		if update.Publication != nil {
			return s.preparePublication(ctx, tx, *update.Publication, action)
		}
		return nil
	}, action.Events...)
}

func (s *Store) Obligations(ctx context.Context, runID string) ([]stateport.Obligation, error) {
	rows, err := s.readers.QueryContext(ctx, `SELECT run_id,kind,value,revision,actor FROM obligations WHERE run_id=? ORDER BY kind`, runID)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]stateport.Obligation, 0)
	for rows.Next() {
		var o stateport.Obligation
		if err := rows.Scan(&o.RunID, &o.Kind, &o.Value, &o.Revision, &o.Actor); err != nil {
			return nil, databaseError(err)
		}
		result = append(result, o)
	}
	return result, databaseError(rows.Err())
}

func decodeNode(body string) (stateport.Node, error) {
	var n stateport.Node
	err := json.Unmarshal([]byte(body), &n)
	return n, err
}

func requireRunBinding(r stateport.Run, binding, expected nativebinding.RunBinding) error {
	if binding.RepositoryID != expected.RepositoryID ||
		binding.Revision.Repository != r.Repo ||
		binding.Revision.Commit != *r.CommitSHA ||
		binding.Ref != r.Ref ||
		binding.Trigger != string(r.Origin.Trigger) ||
		binding.SpecSHA256 != r.Admission.SpecDigest ||
		binding.Identity != r.Admission.Digest {
		return refuse(bindingChanged, nil)
	}
	return nil
}

func validateRunUpdate(update stateport.RunUpdate) error {
	if update.Reason != "" && publicerr.CheckMessage(update.Reason, 40) != nil {
		return refuse(invalid, nil)
	}
	if update.NativePhase != nil && !update.NativePhase.Valid() {
		return refuse(invalid, nil)
	}
	if !update.Status.Valid() || !terminal(update.Status) {
		return refuse(invalid, nil)
	}
	if update.Publication != nil && update.Publication.Publication.RunID != update.ID {
		return refuse(invalid, nil)
	}
	if update.Failure != nil && (!update.Failure.Class.Valid() || !update.Failure.Retry.Action.Valid()) {
		return refuse(invalid, nil)
	}
	return nil
}

func requireFinishable(ctx context.Context, tx *sql.Tx, r stateport.Run, revision int64) error {
	if r.Revision != revision {
		return refuse(staleRevision, nil)
	}

	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM publications WHERE run_id=? AND status='pending'`, r.ID).Scan(&pending); err != nil {
		return databaseError(err)
	}
	if pending > 0 {
		return refuse(pendingPublication, nil)
	}
	if terminal(r.Status) {
		return refuse(terminalRun, nil)
	}
	return nil
}

func (s *Store) finishPromotionFact(ctx context.Context, tx *sql.Tx, action stateport.Action, update stateport.RunUpdate) error {
	if update.Promotion != nil {
		if err := s.finishPromotion(ctx, tx, *update.Promotion); err != nil {
			return err
		}
		if err := s.appendAudit(ctx, tx, mutation{action.Actor, "promotion.finish", "promotion", update.Promotion.ID, nil}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) projectVerdict(ctx context.Context, tx *sql.Tx, r stateport.Run) error {
	err := s.projectCI(ctx, tx, r)
	var same unchanged
	if errors.As(err, &same) {
		return nil
	}
	return err
}

func (s *Store) saveVerdict(ctx context.Context, tx *sql.Tx, r *stateport.Run, update stateport.RunUpdate) error {
	if err := requireFinishable(ctx, tx, *r, update.Revision); err != nil {
		return err
	}
	if err := markArmedFailure(ctx, tx, r); err != nil {
		return err
	}
	r.Status = update.Status
	r.Failure = update.Failure
	r.DisplayReason = update.Reason
	r.Detail = update.Detail
	r.NativePhase = update.NativePhase
	r.FinishedAt = new(s.options.Clock().UTC())
	if update.Restart {
		r.DisplayReason = stateport.ReasonRestart
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,summary=?,origin=?,revision=revision+1 WHERE id=?`, r.Status, encode(r.RunSummary), encode(r.Origin),
		r.ID); err != nil {
		return databaseError(err)
	}
	return nil
}
