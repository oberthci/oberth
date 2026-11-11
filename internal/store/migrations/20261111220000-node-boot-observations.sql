-- Purpose: Retain owned running Pod node boot IDs across server resumes.
-- Rollback: Restore the pre-upgrade database backup.
CREATE TABLE node_boot_observations (
 run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 workflow_name TEXT NOT NULL,
 workflow_uid TEXT NOT NULL,
 node_id TEXT NOT NULL,
 pod_uid TEXT NOT NULL,
 node_name TEXT NOT NULL,
 node_uid TEXT NOT NULL,
 boot_id TEXT NOT NULL,
 started_at INTEGER NOT NULL,
 observed_at INTEGER NOT NULL,
 changed_at INTEGER,
 PRIMARY KEY(run_id, workflow_uid, pod_uid)
) WITHOUT ROWID;
