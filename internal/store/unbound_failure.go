package store

import (
	"context"
	"database/sql"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// FailUnbound serializes against BindWorkflow; only one can change an active run.
// An identical failure retry adds neither audit nor events.
func (s *Store) FailUnbound(ctx context.Context, action stateport.Action, input stateport.UnboundFailure) error {
	return s.change(ctx, mutation{action.Actor, "run.fail.unbound", "run", input.RunID, input.Reason}, func(tx *sql.Tx) error {
		r, err := loadRun(ctx, tx, input.RunID)
		if err != nil {
			return err
		}
		if !r.HasAdmission {
			return refuse(noAdmission, nil)
		}
		if r.Origin.NativeUID() != "" {
			return refuse(workflowBound, nil)
		}
		summary := r.RunSummary
		if err := applyUnboundReason(&summary, input.Reason); err != nil {
			return err
		}
		if terminal(r.Status) {
			if r.Status == stepmodel.RunFailed && r.DisplayReason == input.Reason && r.Detail == input.Detail {
				return unchanged{}
			}
			return refuse(terminalRun, nil)
		}
		summary.Status = stepmodel.RunFailed
		summary.Detail = input.Detail
		summary.FinishedAt = new(s.options.Clock().UTC())
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,summary=?,revision=revision+1 WHERE id=?`, summary.Status,
			encode(summary), r.ID); err != nil {
			return databaseError(err)
		}
		return resolveRemainders(ctx, tx, r.ID)
	}, action.Events...)
}

func applyUnboundReason(summary *stepmodel.RunSummary, reason string) error {
	switch reason {
	case stateport.ReasonCreateRefused:
		summary.DisplayReason = stateport.ReasonCreateRefused
	case stateport.ReasonRetentionRefused:
		summary.DisplayReason = stateport.ReasonRetentionRefused
	case stateport.ReasonRestartAbsent:
		summary.DisplayReason = stateport.ReasonRestartAbsent
	case stateport.ReasonWorkflowOwner:
		summary.DisplayReason = stateport.ReasonWorkflowOwner
	default:
		return refuse(unboundReason, nil)
	}
	return nil
}
