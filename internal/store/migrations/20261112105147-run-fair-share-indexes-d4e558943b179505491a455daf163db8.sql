-- Purpose: Bound fair-share queue lookups independently of repository history.
-- Rollback: Drop runs_repo_active_idx and runs_repo_started_idx, then restore
-- the pre-upgrade database backup before running an older binary.
CREATE INDEX runs_repo_active_idx ON runs(repo_id,status,phase) WHERE status='running';
CREATE INDEX runs_repo_started_idx ON runs(repo_id,started_at DESC) WHERE started_at IS NOT NULL;
