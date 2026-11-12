-- Purpose: remove retired backfill head-wait state.
-- Rollback: restore the pre-upgrade database backup.
DROP TABLE IF EXISTS admission_head_waits;
