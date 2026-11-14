-- Purpose: native Workflow Stop obligations replace automatic supersession.
-- Rollback: Restore the pre-upgrade database backup.
CREATE TABLE run_cancellations_native (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 job_name TEXT NOT NULL DEFAULT '',
 superseded_by TEXT NOT NULL DEFAULT '',
 reason TEXT NOT NULL CHECK (reason IN ('cancelled', 'owner_restart')),
 created_at INTEGER NOT NULL,
 completed_at INTEGER
);
-- Preserve already recorded Stop custody through upgrade. Removing the policy
-- must not abandon a Workflow an earlier owner has already requested to stop.
INSERT INTO run_cancellations_native
 SELECT c.run_id,c.job_name,'',
 CASE WHEN c.reason='owner_restart' THEN 'owner_restart' ELSE 'cancelled' END,
 c.created_at,c.completed_at FROM run_cancellations c;
-- An earlier owner marked cancellation terminal before observing Argo. Restore
-- only those exact outstanding Workflow observations until native Stop finishes.
UPDATE runs SET status='running',phase='cancelled',finished_at=NULL,
 reason='cancelling by oberth: stop requested before upgrade'
 WHERE status='interrupted' AND EXISTS (
 SELECT 1 FROM run_cancellations_native c WHERE c.run_id=runs.id
 AND c.completed_at IS NULL AND c.job_name!='' AND c.reason='cancelled');
DROP TABLE run_cancellations;
ALTER TABLE run_cancellations_native RENAME TO run_cancellations;
CREATE INDEX run_cancellations_pending_idx ON run_cancellations(completed_at,created_at,run_id);
