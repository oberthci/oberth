-- Purpose: Remove the run-level credential classification; current step grants authorize delivery.
-- Rollback: Restore the pre-upgrade database backup.
ALTER TABLE runs DROP COLUMN credentialed;
