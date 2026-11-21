package store

import (
	"context"
	"database/sql"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func (s *Store) writeDeletion(ctx context.Context, tx *sql.Tx, input stateport.BranchDeletion) error {
	request := input.Request
	repoID, err := repositoryNumber(ctx, tx, request.Repo)
	if err != nil {
		return err
	}
	ids, err := cancellableRuns(ctx, tx, repoID, request.Ref)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.cancelDeleted(ctx, tx, id, request.Actor.Identity); err != nil {
			return err
		}
	}
	if err := s.appendAudit(ctx, tx, mutation{request.Actor.Identity, "branch.delete", "repository", request.Repo, request.Ref}); err != nil {
		return err
	}
	return s.writePublication(ctx, tx, input.Publication, "")
}

func (s *Store) cancelDeleted(ctx context.Context, tx *sql.Tx, id, actor string) error {
	r, err := loadRun(ctx, tx, id)
	if err != nil {
		return err
	}
	r.Status = stepmodel.RunCancelled
	r.DisplayReason = stateport.ReasonBranchDeleted
	r.FinishedAt = new(s.options.Clock().UTC())
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status=?,summary=?,revision=revision+1 WHERE id=?`, r.Status, encode(r.RunSummary),
		id); err != nil {
		return databaseError(err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO obligations VALUES(?,'stop',1,'Stop',?) ON CONFLICT(run_id,kind) DO UPDATE SET revision=revision+1,
value=CASE WHEN value='Terminate' THEN value ELSE 'Stop' END,actor=excluded.actor`, id, actor)
	if err != nil {
		return databaseError(err)
	}
	return resolveRemainders(ctx, tx, id)
}

func cancellableRuns(ctx context.Context, tx *sql.Tx, repoID int64, ref string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM runs WHERE repo_id=? AND ref=? AND status IN ('queued','running')`, repoID, ref)
	if err != nil {
		return nil, databaseError(err)
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, databaseError(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, databaseError(err)
	}
	if err := rows.Close(); err != nil {
		return nil, databaseError(err)
	}
	return ids, nil
}

func validateDeletion(input stateport.BranchDeletion) error {
	r, p := input.Request, input.Publication
	if err := validateRequest(r); err != nil {
		return err
	}
	if p.ID == "" || p.Actor != r.Actor.Identity || p.Repo != r.Repo || p.Ref != r.Ref || p.PreviousSHA == nil {
		return refuse(invalid, nil)
	}
	if p.ObjectSHA != "" || p.RunID != "" || p.PromotionID != "" || p.CommitSHA != nil || *p.PreviousSHA != r.PreviousSHA {
		return refuse(invalid, nil)
	}
	return nil
}
