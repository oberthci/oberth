-- Purpose: Native cancel escalation and removal of retired supersede data.
-- Rollback: Restore the pre-upgrade database backup.
ALTER TABLE runs DROP COLUMN superseded_by;
ALTER TABLE run_cancellations DROP COLUMN superseded_by;
ALTER TABLE run_cancellations ADD COLUMN terminate INTEGER NOT NULL DEFAULT 0 CHECK (terminate IN (0,1));
UPDATE run_cancellations SET terminate=1 WHERE reason='cancelled' AND EXISTS (
 SELECT 1 FROM runs r WHERE r.id=run_cancellations.run_id
 AND (r.release=1 OR r.ref_kind='tag' OR r.trigger IN ('promotion','release','apply')));
