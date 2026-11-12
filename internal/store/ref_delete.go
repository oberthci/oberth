package store

import (
	"context"
	"database/sql"
	"fmt"
)

const refDeletedReason = "ref deleted"

// Deletion and receive deduplication share a transaction. A replay therefore
// cannot cancel a run accepted by a later recreation, even at the same SHA.
const deletedBranchRuns = `repo_id=? AND ref_kind='branch' AND ref=?
 AND trigger!='promotion' AND status IN ('queued','running')
 AND NOT EXISTS (SELECT 1 FROM publications WHERE publications.run_id=runs.id AND publications.status='pending')`

func cancelDeletedBranchTx(ctx context.Context, tx *sql.Tx, repoID int64, ref string, now int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_cancellations(run_id,job_name,superseded_by,reason,created_at)
 SELECT id,job_name,'',CASE WHEN phase='infrastructure-pending' THEN 'owner_restart' ELSE 'superseded' END,?
 FROM runs WHERE `+deletedBranchRuns+` AND (status='running' OR job_name!='')
 ON CONFLICT(run_id) DO UPDATE SET job_name=excluded.job_name,superseded_by='',reason=excluded.reason,
 created_at=excluded.created_at,completed_at=NULL`, now, repoID, ref); err != nil {
		return fmt.Errorf("record deleted ref cancellations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status='interrupted',phase='cancelled',reason=?,
 error=CASE WHEN phase='infrastructure-pending' THEN error ELSE ? END,finished_at=?,updated_at=?
 WHERE `+deletedBranchRuns, refDeletedReason, refDeletedReason, now, now, repoID, ref); err != nil {
		return fmt.Errorf("cancel deleted ref runs: %w", err)
	}
	// Retain deferred receipts for idempotent replay, but never drain them into
	// new runs after their ref was deleted. New receives get their own receipt.
	if _, err := tx.ExecContext(ctx, `UPDATE deferred_branch_receives SET reason=? WHERE event_id IN (
 SELECT id FROM receive_events WHERE repo_id=? AND ref_kind='branch' AND ref=? AND run_id='')`, refDeletedReason, repoID, ref); err != nil {
		return fmt.Errorf("cancel deleted ref backlog: %w", err)
	}
	return nil
}
