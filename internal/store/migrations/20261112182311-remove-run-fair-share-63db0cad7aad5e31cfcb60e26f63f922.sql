-- Purpose: Remove Oberth repository fair-share scheduling indexes.
-- Rollback: Restore the pre-upgrade database backup.
DROP INDEX runs_repo_active_idx;
DROP INDEX runs_repo_started_idx;
