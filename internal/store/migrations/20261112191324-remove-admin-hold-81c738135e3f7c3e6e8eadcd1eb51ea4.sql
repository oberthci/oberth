-- Purpose: Remove administrator resource admission holds.
-- Rollback: Restore the pre-upgrade database backup.
DROP TABLE admission_holds;
