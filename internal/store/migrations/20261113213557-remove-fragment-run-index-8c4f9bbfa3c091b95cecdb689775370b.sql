-- Purpose: Remove the fragment-only passed-run lookup index.
-- Rollback: Restore the pre-upgrade database backup.
DROP INDEX IF EXISTS runs_passed_tested_sha_idx;
