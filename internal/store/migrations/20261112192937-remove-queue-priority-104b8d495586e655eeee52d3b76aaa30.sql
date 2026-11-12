-- Purpose: remove queue priority.
-- Rollback: Restore the pre-upgrade database backup.
DROP TABLE queue_priority_patterns;
DROP TABLE run_priority_overrides;
DROP TABLE run_priority_requests;
DROP TABLE priority_grants;
